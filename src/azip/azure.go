package main

import (
	"fmt"
	"math/rand"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/azure-sdk-for-go/arm/examples/helpers"
	"github.com/Azure/azure-sdk-for-go/arm/network"
	"github.com/Azure/go-autorest/autorest/azure"
)

const (
	waitRetries = 1000 // keep retrying
	waitFactor  = 2    // backoff exponent
	waitDefault = 5    // minimum wait
	waitRand    = 10   // random addition to wait time

	k8ipprefix = "k8ip"
	skipVMtag  = "k8skipIP"
)

func backoffExp(f func() error, errPre string) error {
	waitFor := waitDefault + rand.Intn(waitRand)
	for i := 0; i < waitRetries; i++ {
		err := f()
		if err != nil {
			fmt.Println(errPre, err.Error())
		} else {
			return nil
		}
		waitFor = waitFor * waitFactor
		fmt.Printf("Wait for: %d seconds and retry ...\n", waitFor)
		time.Sleep(time.Duration(waitFor) * time.Second)
	}
	return fmt.Errorf("Timeout reached")
}

func initClients(env map[string]string) (network.InterfacesClient, compute.VirtualMachinesClient) {
	rmEndpoint := azure.PublicCloud.ResourceManagerEndpoint
	// handle other endpoints like Azure Gov/China/etc
	if uri := os.Getenv("RESOURCE_MANAGER_ENDPOINT"); uri != "" {
		rmEndpoint = uri
	}

	spt, err := helpers.NewServicePrincipalTokenFromCredentials(env, rmEndpoint)
	if err != nil {
		fmt.Printf("ERROR: Getting SP token - check that all ENV variables are set")
		os.Exit(1)
	}

	// Create Network Interface Client
	nicClient := network.NewInterfacesClientWithBaseURI(rmEndpoint, env["AZURE_SUBSCRIPTION_ID"])
	nicClient.Authorizer = spt
	// Create VM Client
	vmClient := compute.NewVirtualMachinesClientWithBaseURI(rmEndpoint, env["AZURE_SUBSCRIPTION_ID"])
	vmClient.Authorizer = spt

	return nicClient, vmClient
}

func skipVM(vm compute.VirtualMachine) bool {
	if vm.Tags == nil {
		return false
	}
	if _, ok := (*vm.Tags)[skipVMtag]; ok {
		fmt.Println("Tag found on VM: ", skipVMtag)
		return true
	}
	return false
}

func getVM(vmClient compute.VirtualMachinesClient, vmName, groupName string) (*compute.VirtualMachine, error) {
	vm, err := vmClient.Get(groupName, vmName, compute.InstanceView)
	if err != nil {
		fmt.Println("ERROR: failed to get VM details: ", err.Error())
		return nil, err
	}
	fmt.Println("Found VM: ", *vm.ID)
	return &vm, nil
}

func getNIC(nicClient network.InterfacesClient, vm compute.VirtualMachine, groupName string) (*network.Interface, error) {
	var nic network.Interface
	var err error
	nicInterfaces := *vm.VirtualMachineProperties.NetworkProfile.NetworkInterfaces
	nicCount := len(nicInterfaces)
	if nicCount < 1 {
		return nil, fmt.Errorf("ERROR: No NICs found for VM")
	}
	if nicCount == 1 {
		// there is only 1 NIC: no need to look for any tags - just use it
		nicID := *(*vm.VirtualMachineProperties.NetworkProfile.NetworkInterfaces)[0].ID
		fmt.Println("Only one NIC found. Using NIC ID: ", nicID)
		nicName := path.Base(nicID)
		err = backoffExp(func() error {
			nic, err = nicClient.Get(groupName, nicName, "")
			return err
		}, "ERROR: could not get NIC details: ")
		if err != nil {
			return nil, err
		}
		return &nic, nil
	}
	if nicCount > 1 {
		// more than one NIC found. Look for the primary one
		for _, nicRef := range *vm.VirtualMachineProperties.NetworkProfile.NetworkInterfaces {
			nicID := *nicRef.ID
			nicName := path.Base(nicID)
			err = backoffExp(func() error {
				nic, err = nicClient.Get(groupName, nicName, "")
				return err
			}, "ERROR: could not get NIC details: ")
			if err != nil {
				return nil, err
			}
			if (nic.InterfacePropertiesFormat == nil) || ((*nic.InterfacePropertiesFormat).Primary == nil) {
				continue
			}
			if *(*nic.InterfacePropertiesFormat).Primary {
				return &nic, nil
			}
		}
		return nil, fmt.Errorf("ERROR: no primary NIC found")
	}
	return nil, fmt.Errorf("ERROR: No NIC found for k8 usage")
}

func addIPstoVMNic(nicClient network.InterfacesClient, nic network.Interface, groupName string, count int) (err error) {
	newidx := 0
	existingIPs := 0
	var primarySubnet network.Subnet

	for _, ipconfig := range *nic.InterfacePropertiesFormat.IPConfigurations {
		name := *ipconfig.Name
		if strings.HasPrefix(name, k8ipprefix) {
			if idx, err := strconv.Atoi(strings.TrimPrefix(name, k8ipprefix)); err == nil {
				existingIPs = existingIPs + 1
				if idx > newidx {
					newidx = idx
				}
			}
		}

		if (ipconfig.InterfaceIPConfigurationPropertiesFormat == nil) || ((*ipconfig.InterfaceIPConfigurationPropertiesFormat).Primary == nil) {
			continue
		}
		// pick the primary subnet
		if *(*ipconfig.InterfaceIPConfigurationPropertiesFormat).Primary {
			primarySubnet = *(*ipconfig.InterfaceIPConfigurationPropertiesFormat).Subnet
		}
	}

	if existingIPs >= count {
		fmt.Printf("VM already has %d IPs. Skipping addition of new IPs\n", existingIPs)
		return nil
	}

	count = count - existingIPs
	for i := 0; i < count; i++ {
		newidx = newidx + 1
		newIPCfgName := fmt.Sprintf("%s%d", k8ipprefix, newidx)
		fmt.Println("Add new ipcfg ", newIPCfgName)
		newIPCfg := network.InterfaceIPConfiguration{
			Name: &newIPCfgName,
			InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
				PrivateIPAllocationMethod: network.Dynamic,
				Subnet: &primarySubnet,
			},
		}

		*nic.InterfacePropertiesFormat.IPConfigurations = append(*nic.InterfacePropertiesFormat.IPConfigurations, newIPCfg)
	}

	_, err = nicClient.CreateOrUpdate(groupName, *nic.Name, nic, nil)
	fmt.Println("Waiting to update NIC ....")
	err = backoffExp(func() error {
		_, err = nicClient.CreateOrUpdate(groupName, *nic.Name, nic, nil)
		return err
	}, "ERROR: failed to update NIC: ")
	if err != nil {
		return nil
	}
	return nil
}
