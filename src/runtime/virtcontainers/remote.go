// Copyright (c) 2022 IBM Corporation
// SPDX-License-Identifier: Apache-2.0

package virtcontainers

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	cri "github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/containerd/ttrpc"
	persistapi "github.com/kata-containers/kata-containers/src/runtime/pkg/hypervisors"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/device/config"
	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/hypervisor"
	hypannotations "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/annotations"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/types"
	"github.com/pkg/errors"
)

const defaultMinTimeout = 60

// NBD server configuration and state
var (
	nbdServerMutex sync.Mutex
	nbdPortCounter = 10809 // Starting port for NBD servers
	nbdServers     = make(map[string]*types.NBDVolume)
)

type remoteHypervisor struct {
	sandboxID       remoteHypervisorSandboxID
	agentSocketPath string
	config          HypervisorConfig
}

type remoteHypervisorSandboxID string

type remoteService struct {
	conn   net.Conn
	client pb.HypervisorService
}

func openRemoteService(socketPath string) (*remoteService, error) {

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to remote hypervisor socket: %w", err)
	}

	ttrpcClient := ttrpc.NewClient(conn)

	client := pb.NewHypervisorClient(ttrpcClient)

	s := &remoteService{
		conn:   conn,
		client: client,
	}

	return s, nil
}

func (s *remoteService) Close() error {
	return s.conn.Close()
}

func (rh *remoteHypervisor) CreateVM(ctx context.Context, id string, network Network, hypervisorConfig *HypervisorConfig) error {

	rh.sandboxID = remoteHypervisorSandboxID(id)

	if err := rh.setConfig(hypervisorConfig); err != nil {
		return err
	}

	s, err := openRemoteService(hypervisorConfig.RemoteHypervisorSocket)
	if err != nil {
		return err
	}
	defer s.Close()

	annotations := map[string]string{}
	annotations[cri.SandboxName] = hypervisorConfig.SandboxName
	annotations[cri.SandboxNamespace] = hypervisorConfig.SandboxNamespace
	annotations[hypannotations.MachineType] = hypervisorConfig.HypervisorMachineType
	annotations[hypannotations.ImagePath] = hypervisorConfig.ImagePath
	annotations[hypannotations.DefaultVCPUs] = strconv.FormatUint(uint64(hypervisorConfig.NumVCPUs()), 10)
	annotations[hypannotations.DefaultMemory] = strconv.FormatUint(uint64(hypervisorConfig.MemorySize), 10)
	annotations[hypannotations.Initdata] = hypervisorConfig.Initdata
	annotations[hypannotations.DefaultGPUs] = strconv.FormatUint(uint64(hypervisorConfig.DefaultGPUs), 10)
	annotations[hypannotations.DefaultGPUModel] = hypervisorConfig.DefaultGPUModel

	req := &pb.CreateVMRequest{
		Id:                   id,
		Annotations:          annotations,
		NetworkNamespacePath: network.NetworkID(),
	}

	res, err := s.client.CreateVM(ctx, req)
	if err != nil {
		return fmt.Errorf("remote hypervisor call failed: %w", err)
	}

	if res.AgentSocketPath == "" {
		return errors.New("remote hypervisor does not return tunnel socket path")
	}

	rh.agentSocketPath = res.AgentSocketPath

	return nil
}

func (rh *remoteHypervisor) StartVM(ctx context.Context, timeout int) error {

	minTimeout := defaultMinTimeout
	if rh.config.RemoteHypervisorTimeout > 0 {
		minTimeout = int(rh.config.RemoteHypervisorTimeout)
	}

	if timeout < minTimeout {
		timeout = minTimeout
	}

	s, err := openRemoteService(rh.config.RemoteHypervisorSocket)
	if err != nil {
		return err
	}
	defer s.Close()

	req := &pb.StartVMRequest{
		Id: string(rh.sandboxID),
	}

	ctx2, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	hvLogger.Infof("calling remote hypervisor StartVM (timeout: %d)", timeout)

	if _, err := s.client.StartVM(ctx2, req); err != nil {
		return fmt.Errorf("remote hypervisor call failed: %w", err)
	}

	return nil
}

func (rh *remoteHypervisor) AttestVM(ctx context.Context) error {
	return nil
}

func (rh *remoteHypervisor) StopVM(ctx context.Context, waitOnly bool) error {

	s, err := openRemoteService(rh.config.RemoteHypervisorSocket)
	if err != nil {
		return err
	}
	defer s.Close()

	req := &pb.StopVMRequest{
		Id: string(rh.sandboxID),
	}

	if _, err := s.client.StopVM(ctx, req); err != nil {
		return fmt.Errorf("remote hypervisor call failed: %w", err)
	}

	return nil
}

func (rh *remoteHypervisor) GenerateSocket(id string) (interface{}, error) {

	socketPath := rh.agentSocketPath
	if len(socketPath) == 0 {
		return nil, errors.New("failed to generate remote sock: TunnelSocketPath is not set")
	}

	remoteSock := types.RemoteSock{
		SandboxID:        id,
		TunnelSocketPath: socketPath,
	}

	return remoteSock, nil
}

func notImplemented(name string) error {

	err := errors.Errorf("%s: not implemented", name)

	hvLogger.Errorf(err.Error())

	if tracer, ok := err.(interface{ StackTrace() errors.StackTrace }); ok {
		for _, f := range tracer.StackTrace() {
			hvLogger.Errorf("%+s:%d\n", f, f)
		}
	}

	return err
}

func (rh *remoteHypervisor) PauseVM(ctx context.Context) error {
	return notImplemented("PauseVM")
}

func (rh *remoteHypervisor) SaveVM() error {
	return notImplemented("SaveVM")
}

func (rh *remoteHypervisor) ResumeVM(ctx context.Context) error {
	return notImplemented("ResumeVM")
}

func (rh *remoteHypervisor) AddDevice(ctx context.Context, devInfo interface{}, devType DeviceType) error {
	// TODO should we return notImplemented("AddDevice"), rather than nil and ignoring it?
	hvLogger.Infof("addDevice: deviceType=%v devInfo=%#v", devType, devInfo)
	return nil
}

// startNBDServer starts an NBD server for the given block device
func (rh *remoteHypervisor) startNBDServer(blockDrive *config.BlockDrive) (*types.NBDVolume, error) {
	nbdServerMutex.Lock()
	defer nbdServerMutex.Unlock()

	// Get next available port
	port := nbdPortCounter
	nbdPortCounter++

	// Create NBD export name from device ID
	exportName := fmt.Sprintf("export_%s", blockDrive.ID)

	// Start nbdkit server
	cmd := exec.Command("nbdkit", 
		"--foreground",
		"--newstyle", 
		"--exportname", exportName,
		"--port", strconv.Itoa(port),
		"file", blockDrive.File)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start NBD server: %v", err)
	}

	nbdVolume := &types.NBDVolume{
		Server:     "localhost",
		Port:       port,
		ExportName: exportName,
		ServerPID:  cmd.Process.Pid,
		LocalPath:  blockDrive.File,
	}

	// Store NBD server info for cleanup
	nbdServers[blockDrive.ID] = nbdVolume

	hvLogger.WithField("nbd-server", fmt.Sprintf("localhost:%d", port)).
		WithField("export", exportName).
		WithField("local-path", blockDrive.File).
		Info("Started NBD server for block device")

	return nbdVolume, nil
}

// stopNBDServer stops the NBD server for the given device ID
func (rh *remoteHypervisor) stopNBDServer(deviceID string) error {
	nbdServerMutex.Lock()
	defer nbdServerMutex.Unlock()

	nbdVolume, exists := nbdServers[deviceID]
	if !exists {
		return fmt.Errorf("NBD server not found for device %s", deviceID)
	}

	// Kill the NBD server process
	if nbdVolume.ServerPID > 0 {
		if process, err := os.FindProcess(nbdVolume.ServerPID); err == nil {
			if err := process.Kill(); err != nil {
				hvLogger.WithError(err).WithField("pid", nbdVolume.ServerPID).
					Warn("Failed to kill NBD server process")
			}
		}
	}

	delete(nbdServers, deviceID)
	
	hvLogger.WithField("device-id", deviceID).
		WithField("nbd-server", fmt.Sprintf("%s:%d", nbdVolume.Server, nbdVolume.Port)).
		Info("Stopped NBD server for block device")
	
	return nil
}

// createNBDExportPath creates the well-defined path for NBD export info
func (rh *remoteHypervisor) createNBDExportPath(nbdVolume *types.NBDVolume, deviceID string) error {
	exportDir := filepath.Join("/run/kata-containers", string(rh.sandboxID), "nbd-exports")
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		return fmt.Errorf("failed to create NBD export directory: %v", err)
	}

	exportFile := filepath.Join(exportDir, fmt.Sprintf("%s.json", deviceID))
	exportData := fmt.Sprintf(`{
		"server": "%s",
		"port": %d,
		"export_name": "%s",
		"nbd_uri": "nbd://%s:%d/%s"
	}`, nbdVolume.Server, nbdVolume.Port, nbdVolume.ExportName, 
		nbdVolume.Server, nbdVolume.Port, nbdVolume.ExportName)

	if err := os.WriteFile(exportFile, []byte(exportData), 0644); err != nil {
		return fmt.Errorf("failed to write NBD export info: %v", err)
	}

	hvLogger.WithField("export-file", exportFile).
		Info("Created NBD export info file")

	return nil
}

func (rh *remoteHypervisor) HotplugAddDevice(ctx context.Context, devInfo interface{}, devType DeviceType) (interface{}, error) {
	switch devType {
	case BlockDev:
		blockDrive, ok := devInfo.(*config.BlockDrive)
		if !ok {
			return nil, fmt.Errorf("device type mismatch, expect *config.BlockDrive for BlockDev")
		}

		hvLogger.WithField("block-drive", blockDrive).
			Info("Hotplugging block device for remote hypervisor")

		// Start NBD server for the block device
		nbdVolume, err := rh.startNBDServer(blockDrive)
		if err != nil {
			return nil, fmt.Errorf("failed to start NBD server for block device %s: %v", blockDrive.ID, err)
		}

		// Create well-defined path for NBD export info
		if err := rh.createNBDExportPath(nbdVolume, blockDrive.ID); err != nil {
			// Clean up NBD server if export path creation fails
			rh.stopNBDServer(blockDrive.ID)
			return nil, fmt.Errorf("failed to create NBD export path: %v", err)
		}

		// Create KataVirtualVolume with NBD info
		kataVolume := &types.KataVirtualVolume{
			VolumeType: types.KataVirtualVolumeNBDType,
			Source:     fmt.Sprintf("nbd://%s:%d/%s", nbdVolume.Server, nbdVolume.Port, nbdVolume.ExportName),
			NBD:        nbdVolume,
		}

		hvLogger.WithField("kata-volume", kataVolume).
			Info("Created KataVirtualVolume with NBD info")

		return kataVolume, nil

	default:
		return nil, fmt.Errorf("device type %v not supported by remote hypervisor", devType)
	}
}

func (rh *remoteHypervisor) HotplugRemoveDevice(ctx context.Context, devInfo interface{}, devType DeviceType) (interface{}, error) {
	switch devType {
	case BlockDev:
		blockDrive, ok := devInfo.(*config.BlockDrive)
		if !ok {
			return nil, fmt.Errorf("device type mismatch, expect *config.BlockDrive for BlockDev")
		}

		hvLogger.WithField("device-id", blockDrive.ID).
			Info("Removing block device from remote hypervisor")

		// Stop NBD server
		if err := rh.stopNBDServer(blockDrive.ID); err != nil {
			hvLogger.WithError(err).WithField("device-id", blockDrive.ID).
				Warn("Failed to stop NBD server during device removal")
		}

		// Clean up export info file
		exportDir := filepath.Join("/run/kata-containers", string(rh.sandboxID), "nbd-exports")
		exportFile := filepath.Join(exportDir, fmt.Sprintf("%s.json", blockDrive.ID))
		if err := os.Remove(exportFile); err != nil {
			hvLogger.WithError(err).WithField("export-file", exportFile).
				Warn("Failed to remove NBD export info file")
		}

		return nil, nil

	default:
		return nil, fmt.Errorf("device type %v not supported by remote hypervisor", devType)
	}
}

func (rh *remoteHypervisor) ResizeMemory(ctx context.Context, memMB uint32, memoryBlockSizeMB uint32, probe bool) (uint32, MemoryDevice, error) {
	return memMB, MemoryDevice{}, nil
}

func (rh *remoteHypervisor) GetTotalMemoryMB(ctx context.Context) uint32 {
	//The remote hypervisor uses the peer pod config to determine the memory of the VM, so we need to use static resource management
	hvLogger.Error("GetTotalMemoryMB - remote hypervisor cannot update resources")
	return 0
}

func (rh *remoteHypervisor) ResizeVCPUs(ctx context.Context, vcpus uint32) (uint32, uint32, error) {
	return vcpus, vcpus, nil
}

func (rh *remoteHypervisor) GetVMConsole(ctx context.Context, sandboxID string) (string, string, error) {
	return "", "", notImplemented("GetVMConsole")
}

func (rh *remoteHypervisor) Disconnect(ctx context.Context) {
	notImplemented("Disconnect")
}

func (rh *remoteHypervisor) Capabilities(ctx context.Context) types.Capabilities {
	var caps types.Capabilities
	caps.SetBlockDeviceHotplugSupport()
	return caps
}

func (rh *remoteHypervisor) HypervisorConfig() HypervisorConfig {
	return rh.config
}

func (rh *remoteHypervisor) GetThreadIDs(ctx context.Context) (VcpuThreadIDs, error) {
	// Not supported. return success
	// Just allocating an empty map
	return VcpuThreadIDs{}, nil
}

func (rh *remoteHypervisor) Cleanup(ctx context.Context) error {
	return nil
}

func (rh *remoteHypervisor) setConfig(config *HypervisorConfig) error {
	// Create a Validator specific for remote hypervisor
	rh.config = *config

	return nil
}

func (rh *remoteHypervisor) GetPids() []int {
	// let's use shim pid as it used by crio to fetch start time
	return []int{os.Getpid()}
}

func (rh *remoteHypervisor) GetVirtioFsPid() *int {
	return nil
}

func (rh *remoteHypervisor) fromGrpc(ctx context.Context, hypervisorConfig *HypervisorConfig, j []byte) error {
	panic(notImplemented("fromGrpc"))
}

func (rh *remoteHypervisor) toGrpc(ctx context.Context) ([]byte, error) {
	panic(notImplemented("toGrpc"))
}

func (rh *remoteHypervisor) Check() error {
	return nil
}

func (rh *remoteHypervisor) Save() persistapi.HypervisorState {
	return persistapi.HypervisorState{}
}

func (rh *remoteHypervisor) Load(persistapi.HypervisorState) {
	notImplemented("Load")
}

func (rh *remoteHypervisor) IsRateLimiterBuiltin() bool {
	return false
}
