package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

func attach(flagUsername, name string) error {
	file, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("machine already attached")
	}

	username, password, err := readCredentials(flagUsername)
	if err != nil {
		return err
	}

	machineName, err := resolveMachineName(name)
	if err != nil {
		return err
	}

	return deskconn.Attach(username, password, machineName)
}

func resolveMachineName(name string) (string, error) {
	if name != "" {
		return name, nil
	}

	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to get hostname: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Name of the new machine [default=%s]: ", host)

	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return host, nil
	}
	return input, nil
}

func promptAttachMachine(username, password string) error {
	file, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return nil
	}

	confirmed, err := confirmPrompt("No machines found on your account. Attach this machine now? (Y/n): ", true)
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}

	machineName, err := resolveMachineName("")
	if err != nil {
		return err
	}

	if err := deskconn.Attach(username, password, machineName); err != nil {
		return err
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		return err
	}

	machines, err := deskconn.FetchMachinesFromCloud(cfgDirectory)
	if err != nil {
		return fmt.Errorf("failed to fetch machines: %w", err)
	}

	return deskconn.CacheMachines(cfgDirectory, machines)
}

// detach detaches this machine from the user's account after they confirm, and removes
// its credentials so it can be attached again.
func detach(cfgDirectory string) error {
	machineID, err := xconnd.MachineID()
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}

	session, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()

	fmt.Println("This machine will be removed from your account and can't be reached remotely " +
		"until you attach it again.")
	confirmed, err := confirmPrompt("Detach this machine? (y/N): ", false)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("detach cancelled")
		return nil
	}

	if err := deskconn.Detach(session, machineID); err != nil {
		return err
	}

	// userd also removes it when it hears about the detach, but not if it isn't running,
	// and attach refuses to run while it's there.
	credFile, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(credFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("machine detached, but failed to remove its credentials: %w", err)
	}

	fmt.Println("Machine detached.")
	return nil
}

// detachIfAttached detaches this machine from the cloud account if it is currently
// attached. It is a no-op otherwise.
func detachIfAttached(cfgDirectory string) error {
	credFile, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(credFile); os.IsNotExist(err) {
		return nil
	}
	machineID, err := xconnd.MachineID()
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}

	session, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()

	fmt.Println("Detaching this machine from your account...")
	if err := deskconn.Detach(session, machineID); err != nil {
		return err
	}
	fmt.Println("Machine detached.")
	return nil
}
