// +build darwin

/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hyperkit

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"regexp"

	nfsexports "github.com/johanneswuerbach/nfsexports"
	pkgdrivers "github.com/leoh0/docker-machine-driver-hyperkit/pkg/drivers"
	"github.com/leoh0/machine/libmachine/drivers"
	"github.com/leoh0/machine/libmachine/log"
	"github.com/leoh0/machine/libmachine/mcnutils"
	"github.com/leoh0/machine/libmachine/state"
	ps "github.com/mitchellh/go-ps"
	hyperkit "github.com/moby/hyperkit/go"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
)

const (
	isoFilename     = "boot2docker.iso"
	isoMountPath    = "b2d-image"
	pidFileName     = "hyperkit.pid"
	machineFileName = "hyperkit.json"
	permErr         = "%s needs to run with elevated permissions. " +
		"Please run the following command, then try again: " +
		"sudo chown root:wheel %s && sudo chmod u+s %s"
	defaultSSHUser = "docker"
)

var (
	kernelRegexp       = regexp.MustCompile(`(vmlinu[xz]|bzImage)[\d]*`)
	kernelOptionRegexp = regexp.MustCompile(`(?:\t|\s{2})append\s+([[:print:]]+)`)
)

type Driver struct {
	*drivers.BaseDriver
	*pkgdrivers.CommonDriver
	Boot2DockerURL string
	DiskSize       int
	CPU            int
	Memory         int
	Cmdline        string
	NFSShares      []string
	NFSSharesRoot  string
	UUID           string
	BootKernel string
	BootInitrd string
	Initrd     string
	Vmlinuz    string
}

// Return the state of the hyperkit pid
func pidState(pid int) (state.State, error) {
	if pid == 0 {
		return state.Stopped, nil
	}
	p, err := ps.FindProcess(pid)
	if err != nil {
		return state.Error, err
	}
	if p == nil {
		log.Debugf("hyperkit pid %d missing from process table", pid)
		return state.Stopped, nil
	}
	// hyperkit or com.docker.hyper
	if !strings.Contains(p.Executable(), "hyper") {
		log.Debugf("pid %d is stale, and is being used by %s", pid, p.Executable())
		return state.Stopped, nil
	}
	return state.Running, nil
}

func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			SSHUser: defaultSSHUser,
		},
		CPU: 2,
		Memory: 6000,
		DiskSize: 20000,
		UUID: string(uuid.NewUUID()),
		CommonDriver: &pkgdrivers.CommonDriver{},
	}
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = defaultSSHUser
	}

	return d.SSHUser
}

// PreCreateCheck is called to enforce pre-creation steps
func (d *Driver) PreCreateCheck() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if syscall.Geteuid() != 0 {
		return fmt.Errorf(permErr, filepath.Base(exe), exe, exe)
	}

	return nil
}

func (d *Driver) Create() error {
	// TODO: handle different disk types.
	if err := pkgdrivers.MakeDiskImage(d.BaseDriver, d.Boot2DockerURL, d.DiskSize); err != nil {
		return errors.Wrap(err, "making disk image")
	}

	isoPath := d.ResolveStorePath(isoFilename)
	if err := d.extractKernel(isoPath); err != nil {
		return err
	}

	return d.Start()
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return "hyperkit"
}

// GetSSHHostname returns hostname for use with ssh
func (d *Driver) GetSSHHostname() (string, error) {
	return d.IPAddress, nil
}

// GetURL returns a Docker compatible host URL for connecting to this host
// e.g. tcp://1.2.3.4:2376
func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

// GetState returns the state that the host is in (running, stopped, etc)
func (d *Driver) GetState() (state.State, error) {
	pid := d.getPid()
	if pid == 0 {
		return state.Stopped, nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return state.Error, err
	}

	// Sending a signal of 0 can be used to check the existence of a process.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return state.Stopped, nil
	}
	if p == nil {
		return state.Stopped, nil
	}
	return state.Running, nil
}

// Kill stops a host forcefully
func (d *Driver) Kill() error {
	return d.sendSignal(syscall.SIGKILL)
}

// Remove a host
func (d *Driver) Remove() error {
	s, err := d.GetState()
	if err != nil || s == state.Error {
		log.Infof("Error checking machine status: %s, assuming it has been removed already", err)
	}
	if s == state.Running {
		if err := d.Stop(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) Restart() error {
	return pkgdrivers.Restart(d)
}

// Start a host
func (d *Driver) Start() error {
	if err := d.recoverFromUncleanShutdown(); err != nil {
		return err
	}

	stateDir := filepath.Join(d.StorePath, "machines", d.MachineName)
	h, err := hyperkit.New("", "", stateDir)
	if err != nil {
		return err
	}

	// TODO: handle the rest of our settings.
	h.Kernel = d.ResolveStorePath(d.Vmlinuz)
	h.Initrd = d.ResolveStorePath(d.Initrd)
	h.VMNet = true
	h.ISOImages = []string{d.ResolveStorePath(isoFilename)}
	h.Console = hyperkit.ConsoleFile
	h.CPUs = d.CPU
	h.Memory = d.Memory
	h.UUID = d.UUID

	h.Disks = []hyperkit.Disk{
		&hyperkit.RawDisk{
			Path: pkgdrivers.GetDiskPath(d.BaseDriver),
			Size: d.DiskSize,
			Trim: true,
		},
	}

	log.Infof("Using UUID %s", h.UUID)
	mac, err := GetMACAddressFromUUID(h.UUID)
	if err != nil {
		return err
	}

	// Need to strip 0's
	mac = trimMacAddress(mac)
	log.Infof("Generated MAC %s", mac)
	log.Infof("Starting with cmdline: %s", d.Cmdline)
	if _, err := h.Start(d.Cmdline); err != nil {
		return err
	}

	getIP := func() error {
		var err error
		d.IPAddress, err = GetIPAddressByMACAddress(mac)
		if err != nil {
			return &RetriableError{Err: err}
		}
		return nil
	}

	if err := RetryAfter(30, getIP, 2*time.Second); err != nil {
		return fmt.Errorf("IP address never found in dhcp leases file %v", err)
	}

	if len(d.NFSShares) > 0 {
		log.Info("Setting up NFS mounts")

		// takes some time here for ssh / nfsd to work properly
		err = d.waitForIP()
		if err != nil {
			log.Errorf("Failed to get IP address for VM: %s", err.Error())
			return err
		}

		err = d.setupNFSShare()
		if err != nil {
			log.Errorf("NFS setup failed: %s", err.Error())
			return err
		}
	}

	return nil
}

// Stop a host gracefully
func (d *Driver) Stop() error {
	d.cleanupNfsExports()
	return d.sendSignal(syscall.SIGTERM)
}

func (d *Driver) extractKernel(isoPath string) error {
	log.Debugf("Mounting %s", isoFilename)

	volumeRootDir := d.ResolveStorePath(isoMountPath)
	err := hdiutil("attach", d.ResolveStorePath(isoFilename), "-mountpoint", volumeRootDir)
	if err != nil {
		return err
	}
	defer func() error {
		log.Debugf("Unmounting %s", isoFilename)
		return hdiutil("detach", volumeRootDir)
	}()

	log.Debugf("Extracting Kernel Options...")
	if err := d.extractKernelOptions(); err != nil {
		return err
	}

	if d.BootKernel == "" && d.BootInitrd == "" {
		filepath.Walk(volumeRootDir, func(path string, f os.FileInfo, err error) error {
			if kernelRegexp.MatchString(path) {
				d.BootKernel = path
				_, d.Vmlinuz = filepath.Split(path)
			}
			if strings.Contains(path, "initrd") {
				d.BootInitrd = path
				_, d.Initrd = filepath.Split(path)
			}
			return nil
		})
	}
	
	if  d.BootKernel == "" || d.BootInitrd == "" {
		err := fmt.Errorf("==== Can't extract Kernel and Ramdisk file ====")
		return err
		}

	dest := d.ResolveStorePath(d.Vmlinuz)
	log.Debugf("Extracting %s into %s", d.BootKernel, dest)
	if err := mcnutils.CopyFile(d.BootKernel, dest); err != nil {
		return err
	}

	dest = d.ResolveStorePath(d.Initrd)
	log.Debugf("Extracting %s into %s", d.BootInitrd, dest)
	if err := mcnutils.CopyFile(d.BootInitrd, dest); err != nil {
		return err
	}

	return nil
}

func (d *Driver) setupNFSShare() error {
	user, err := user.Current()
	if err != nil {
		return err
	}

	hostIP, err := GetNetAddr()
	if err != nil {
		return err
	}

	mountCommands := fmt.Sprintf("#/bin/bash\\n")
	log.Info(d.IPAddress)

	for _, share := range d.NFSShares {
		if !path.IsAbs(share) {
			share = d.ResolveStorePath(share)
		}
		nfsConfig := fmt.Sprintf("%s %s -alldirs -mapall=%s", share, d.IPAddress, user.Username)

		if _, err := nfsexports.Add("", d.nfsExportIdentifier(share), nfsConfig); err != nil {
			if strings.Contains(err.Error(), "conflicts with existing export") {
				log.Info("Conflicting NFS Share not setup and ignored:", err)
				continue
			}
			return err
		}

		root := d.NFSSharesRoot
		mountCommands += fmt.Sprintf("sudo mkdir -p %s/%s\\n", root, share)
		mountCommands += fmt.Sprintf("sudo mount -t nfs -o noacl,async %s:%s %s/%s\\n", hostIP, share, root, share)
	}

	if err := nfsexports.ReloadDaemon(); err != nil {
		return err
	}

	writeScriptCmd := fmt.Sprintf("echo -e \"%s\" | sh", mountCommands)

	if _, err := drivers.RunSSHCommandFromDriver(d, writeScriptCmd); err != nil {
		return err
	}

	return nil
}

// recoverFromUncleanShutdown searches for an existing hyperkit.pid file in
// the machine directory. If it can't find it, a clean shutdown is assumed.
// If it finds the pid file, it checks for a running hyperkit process with that pid
// as the existence of a file might not indicate an unclean shutdown but an actual running
// hyperkit server. This is an error situation - we shouldn't start minikube as there is likely
// an instance running already. If the PID in the pidfile does not belong to a running hyperkit
// process, we can safely delete it, and there is a good chance the machine will recover when restarted.
func (d *Driver) recoverFromUncleanShutdown() error {
	stateDir := filepath.Join(d.StorePath, "machines", d.MachineName)
	pidFile := filepath.Join(stateDir, pidFileName)

	if _, err := os.Stat(pidFile); err != nil {
		if os.IsNotExist(err) {
			log.Debugf("clean start, hyperkit pid file doesn't exist: %s", pidFile)
			return nil
		}
		return errors.Wrap(err, "stat")
	}

	log.Warnf("minikube might have been shutdown in an unclean way, the hyperkit pid file still exists: %s", pidFile)
	bs, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return errors.Wrapf(err, "reading pidfile %s", pidFile)
	}
	content := strings.TrimSpace(string(bs))
	pid, err := strconv.Atoi(content)
	if err != nil {
		return errors.Wrapf(err, "parsing pidfile %s", pidFile)
	}

	st, err := pidState(pid)
	if err != nil {
		return errors.Wrap(err, "pidState")
	}

	log.Debugf("pid %d is in state %q", pid, st)
	if st == state.Running {
		return nil
	}
	log.Debugf("Removing stale pid file %s...", pidFile)
	if err := os.Remove(pidFile); err != nil {
		return errors.Wrap(err, fmt.Sprintf("removing pidFile %s", pidFile))
	}
	return nil
}

func (d *Driver) nfsExportIdentifier(path string) string {
	return fmt.Sprintf("minikube-hyperkit %s-%s", d.MachineName, path)
}

func (d *Driver) sendSignal(s os.Signal) error {
	pid := d.getPid()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	return proc.Signal(s)
}

func (d *Driver) getPid() int {
	pidPath := d.ResolveStorePath(machineFileName)

	f, err := os.Open(pidPath)
	if err != nil {
		log.Warnf("Error reading pid file: %s", err)
		return 0
	}
	dec := json.NewDecoder(f)

	var config struct {
		Pid int `json:"pid"`
	}

	if err := dec.Decode(&config); err != nil {
		log.Warnf("Error decoding pid file: %s", err)
		return 0
	}

	return config.Pid
}

func (d *Driver) cleanupNfsExports() {
	if len(d.NFSShares) > 0 {
		log.Infof("You must be root to remove NFS shared folders. Please type root password.")
		for _, share := range d.NFSShares {
			if _, err := nfsexports.Remove("", d.nfsExportIdentifier(share)); err != nil {
				log.Errorf("failed removing nfs share (%s): %s", share, err.Error())
			}
		}

		if err := nfsexports.ReloadDaemon(); err != nil {
			log.Errorf("failed to reload the nfs daemon: %s", err.Error())
		}
	}
}

func (d *Driver) extractKernelOptions() error {
	volumeRootDir := d.ResolveStorePath(isoMountPath)
	if d.Cmdline == "" {
		err := filepath.Walk(volumeRootDir, func(path string, f os.FileInfo, err error) error {
			if strings.Contains(path, "isolinux.cfg") {
				d.Cmdline, err = readLine(path)
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		if d.Cmdline == "" {
			return errors.New("Not able to parse isolinux.cfg")
		}
	}

	log.Debugf("Extracted Options %q", d.Cmdline)
	return nil
}

func (d *Driver) waitForIP() error {
	var ip string
	var err error
	mac, err := GetMACAddressFromUUID(d.UUID)
	if err != nil {
		return err
	}

	log.Infof("Waiting for VM to come online...")
	for i := 1; i <= 60; i++ {

		ip, err = GetIPAddressByMACAddress(mac)
		if err != nil {
			log.Debugf("Not there yet %d/%d, error: %s", i, 60, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if ip != "" {
			log.Debugf("Got an ip: %s", ip)
			d.IPAddress = ip

			break
		}
	}

	if ip == "" {
		return fmt.Errorf("Machine didn't return an IP after 120 seconds, aborting")
	}

	// Wait for SSH over NAT to be available before returning to user
	if err := drivers.WaitForSSH(d); err != nil {
		return err
	}

	return nil
}
