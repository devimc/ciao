/*
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"os/user"
	"path"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/ciao-project/ciao/clogger/gloginterface"
	"github.com/ciao-project/ciao/networking/libsnnet"
	"github.com/ciao-project/ciao/osprepare"
	"github.com/ciao-project/ciao/payloads"
	"github.com/ciao-project/ciao/ssntp"
	"github.com/golang/glog"
)

var profileFN func() func()
var traceFN func() func()

type uiFlag string

func (f *uiFlag) String() string {
	return string(*f)
}

func (f *uiFlag) Set(val string) error {
	if val != "none" && val != "nc" && val != "spice" {
		return fmt.Errorf("none, nc or spice expected")
	}
	*f = uiFlag(val)

	return nil
}

func (f *uiFlag) Enabled() bool {
	return string(*f) != "none"
}

type qemuVirtualisationFlag string

func (f *qemuVirtualisationFlag) String() string {
	return string(*f)
}

func (f *qemuVirtualisationFlag) Set(val string) error {
	if val != "auto" && val != "kvm" && val != "software" {
		return fmt.Errorf("auto, kvm, or software")
	}
	*f = qemuVirtualisationFlag(val)

	return nil
}

var netConfig networkConfig
var serverCertPath string
var clientCertPath string
var networking bool
var hardReset bool
var diskLimit bool
var memLimit bool
var cephID string
var simulate bool
var childProcessCreds *syscall.SysProcAttr
var childProcessKVMCreds *syscall.SysProcAttr
var maxInstances = int(math.MaxInt32)

func init() {
	flag.StringVar(&serverCertPath, "cacert", "", "Client certificate")
	flag.StringVar(&clientCertPath, "cert", "", "CA certificate")
	flag.BoolVar(&networking, "network", true, "Enable networking")
	flag.BoolVar(&hardReset, "hard-reset", false, "Kill and delete all instances, reset networking and exit")
	flag.BoolVar(&simulate, "simulation", false, "Launcher simulation")
	flag.StringVar(&cephID, "ceph_id", "", "ceph client id")
}

const (
	lockDir         = "/tmp/lock/ciao"
	ciaoDir         = "/var/lib/ciao"
	instancesDir    = ciaoDir + "/instances"
	dataDir         = ciaoDir + "/data/launcher/"
	logDir          = ciaoDir + "/logs/launcher"
	maintenanceFile = dataDir + "/maintenance"
	networkFile     = dataDir + "/network"
	instanceState   = "state"
	lockFile        = "client-agent.lock"
	statsPeriod     = 6
	resourcePeriod  = 30
)

func installLauncherDeps(role ssntp.Role, doneCh chan struct{}) {
	ctx, cancelFunc := context.WithCancel(context.Background())

	ch := make(chan error)
	go func() {

		logger := gloginterface.CiaoGlogLogger{}
		osprepare.Bootstrap(ctx, logger)

		launcherDeps := osprepare.NewPackageRequirements()

		if role.IsNetAgent() {
			launcherDeps.Append(launcherNetNodeDeps)
		}
		if role.IsAgent() {
			launcherDeps.Append(launcherComputeNodeDeps)
		}

		osprepare.InstallDeps(ctx, launcherDeps, logger)

		ch <- nil
	}()

	select {
	case <-doneCh:
		glog.Info("Received terminating signal.  Cancelling installation of launcher dependencies.")
		cancelFunc()
		<-ch
	case err := <-ch:
		if err != nil {
			glog.Errorf("Failed to install launcher dependencies: %v\n", err)
		}
		cancelFunc()
	}
}

func insCmdChannel(instance string, ovsCh chan<- interface{}) chan<- interface{} {
	targetCh := make(chan ovsGetResult)
	ovsCh <- &ovsGetCmd{instance, targetCh}
	target := <-targetCh
	return target.cmdCh
}

func getAllInstances(ovsCh chan<- interface{}) []ovsInstance {
	targetCh := make(chan ovsGetAllResult)
	ovsCh <- &ovsGetAllCmd{targetCh}
	target := <-targetCh
	return target.instances
}

func insState(instance string, ovsCh chan<- interface{}) ovsGetResult {
	targetCh := make(chan ovsGetResult)
	ovsCh <- &ovsGetCmd{instance, targetCh}
	return <-targetCh
}

func processCommand(conn serverConn, cmd *cmdWrapper, ovsCh chan<- interface{}) {
	if cmd.instance != "" {
		processInstanceCommand(conn, cmd, ovsCh)
		return
	}

	switch cmd.cmd.(type) {
	case *statusCmd:
		ovsCh <- &ovsStatsStatusCmd{}
		return
	case *evacuateCmd:
		doneCh := make(chan struct{})
		ovsCh <- &ovsMaintenanceCmd{doneCh}
		<-doneCh
		var wg sync.WaitGroup
		for _, i := range getAllInstances(ovsCh) {
			wg.Add(1)
			go func(i ovsInstance) {
				i.cmdCh <- &insDeleteCmd{
					stop: true,
				}
				errCh := make(chan error)
				ovsCh <- &ovsRemoveCmd{i.instance, errCh}
				<-errCh
				wg.Done()
			}(i)
		}
		wg.Wait()
		glog.Info("All instances evacuated")
	case *restoreCmd:
		doneCh := make(chan struct{})
		ovsCh <- &ovsRestoreCmd{doneCh}
		<-doneCh
		glog.Info("Node restored")
	}
}

func processInstanceCommand(conn serverConn, cmd *cmdWrapper, ovsCh chan<- interface{}) {
	var target chan<- interface{}
	var delCmd *insDeleteCmd

	switch insCmd := cmd.cmd.(type) {
	case *insStartCmd:
		targetCh := make(chan ovsAddResult)
		ovsCh <- &ovsAddCmd{cmd.instance, insCmd.cfg, targetCh}
		addResult := <-targetCh
		if addResult.errorCode != "" {
			if addResult.errorCode == payloads.FullComputeNode {
				glog.Errorf("Instance %s will make node full: Disk %d Mem %d CPUs %d",
					insCmd.cfg.Instance, insCmd.cfg.Disk, insCmd.cfg.Mem,
					insCmd.cfg.Cpus)
			} else {
				glog.Errorf("Node in maintenance mode.  Instance %s cannot be launched",
					insCmd.cfg.Instance)
			}
			se := startError{nil, addResult.errorCode, insCmd.cfg.Restart}
			se.send(conn, cmd.instance)
			return
		}
		target = addResult.cmdCh
	case *insDeleteCmd:
		insState := insState(cmd.instance, ovsCh)
		target = insState.cmdCh
		if target == nil {
			glog.Errorf("Instance %s does not exist", cmd.instance)
			de := deleteError{nil, payloads.DeleteNoInstance}
			de.send(conn, cmd.instance)
			return
		}
		delCmd = insCmd
	default:
		target = insCmdChannel(cmd.instance, ovsCh)
	}

	if target == nil {
		glog.Errorf("Instance %s does not exist", cmd.instance)
		return
	}

	target <- cmd.cmd

	if delCmd != nil {
		errCh := make(chan error)
		ovsCh <- &ovsRemoveCmd{
			cmd.instance,
			errCh}
		<-errCh
	}
}

func startNetwork(doneCh chan struct{}) error {
	if networking {
		ctx, cancelFunc := context.WithCancel(context.Background())
		ch := initNetworking(ctx)
		select {
		case <-doneCh:
			glog.Info("Received terminating signal.  Quitting")
			cancelFunc()
			return fmt.Errorf("init network cancelled")
		case err := <-ch:
			cancelFunc()
			if err != nil {
				glog.Errorf("Failed to init network: %v\n", err)
				return err
			}
		}
	}
	return nil
}

func loadClusterConfig(conn serverConn) error {
	clusterConfig, err := conn.ClusterConfiguration()
	if err != nil {
		return err
	}
	netConfig.ComputeNet = clusterConfig.Configure.Launcher.ComputeNetwork
	netConfig.MgmtNet = clusterConfig.Configure.Launcher.ManagementNetwork
	diskLimit = clusterConfig.Configure.Launcher.DiskLimit
	memLimit = clusterConfig.Configure.Launcher.MemoryLimit
	if cephID == "" {
		cephID = clusterConfig.Configure.Storage.CephID
	}

	childUser := clusterConfig.Configure.Launcher.ChildUser
	if childUser != "" {
		usr, err := user.Lookup(childUser)
		if err != nil {
			return err
		}
		uid, err := strconv.Atoi(usr.Uid)
		if err != nil {
			return err
		}
		grp, err := user.LookupGroup(childUser)
		if err != nil {
			return err
		}
		gid, err := strconv.Atoi(grp.Gid)
		if err != nil {
			return err
		}
		childProcessCreds = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(uid),
				Gid: uint32(gid),
			},
		}
		grp, err = user.LookupGroup("kvm")
		if err != nil {
			return err
		}
		kgid, err := strconv.Atoi(grp.Gid)
		if err != nil {
			return err
		}
		grp, err = user.LookupGroup("disk")
		if err != nil {
			return err
		}
		dgid, err := strconv.Atoi(grp.Gid)
		if err != nil {
			return err
		}

		childProcessKVMCreds = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid:    uint32(uid),
				Gid:    uint32(gid),
				Groups: []uint32{uint32(kgid), uint32(dgid)},
			},
		}
	}

	if err := netConfig.Save(); err != nil {
		glog.Warningf("Unable to save networking config: %v", err)
	}

	return nil
}

func printClusterConfig() {
	glog.Info("Cluster Configuration")
	glog.Info("-----------------------")
	glog.Infof("Compute Network:      %v", netConfig.ComputeNet)
	glog.Infof("Management Network:   %v", netConfig.MgmtNet)
	glog.Infof("Disk Limit:           %v", diskLimit)
	glog.Infof("Memory Limit:         %v", memLimit)
	glog.Infof("Ceph ID:              %v", cephID)
	if childProcessCreds != nil {
		glog.Infof("Credentials:          %d:%d",
			childProcessCreds.Credential.Uid,
			childProcessCreds.Credential.Gid)
	}
}

func connectToServer(doneCh chan struct{}, statusCh chan struct{}) {

	defer func() {
		statusCh <- struct{}{}
	}()

	var wg sync.WaitGroup

	cfg := &ssntp.Config{CAcert: serverCertPath, Cert: clientCertPath,
		Log: ssntp.Log}
	client := &agentClient{
		conn:  &ssntpConn{},
		cmdCh: make(chan *cmdWrapper),
	}

	var ovsCh chan<- interface{}

	dialCh := make(chan error)

	go func() {
		err := client.conn.Dial(cfg, client)
		if err != nil {
			glog.Errorf("Unable to connect to server %v", err)
		}

		dialCh <- err
	}()

	select {
	case err := <-dialCh:
		if err != nil {
			break
		}

		role := client.conn.Role()
		if !(role.IsNetAgent() || role.IsAgent()) {
			glog.Errorf("Invalid certificate role: %s", role.String())
			client.conn.Close()
			return
		}

		err = loadClusterConfig(client.conn)
		if err != nil {
			glog.Errorf("Unable to get Cluster Configuration %v", err)
			client.conn.Close()
			return
		}
		printClusterConfig()

		installLauncherDeps(client.conn.Role(), doneCh)

		err = startNetwork(doneCh)
		if err != nil {
			glog.Errorf("Failed to start network: %v\n", err)
			client.conn.Close()
			return
		}
		defer shutdownNetwork()

		ovsCh = startOverseer(&wg, client)
	case <-doneCh:
		client.conn.Close()
		<-dialCh
		return
	}

DONE:
	for {
		select {
		case <-doneCh:
			client.conn.Close()
			break DONE
		case cmd := <-client.cmdCh:
			/*
				Double check we're not quitting here.  Otherwise a flood of commands
				from the server could block our exit for an arbitrary amount of time,
				i.e, doneCh and cmdCh could become available at the same time.
			*/
			select {
			case <-doneCh:
				client.conn.Close()
				break DONE
			default:
			}

			processCommand(client.conn, cmd, ovsCh)
		}
	}

	if ovsCh != nil {
		close(ovsCh)
	}
	wg.Wait()
	glog.Info("Overseer has closed down")
}

func getLock() error {
	err := os.MkdirAll(lockDir, 0777)
	if err != nil {
		glog.Errorf("Unable to create lockdir %s", lockDir)
		return err
	}

	/* We're going to let the OS close and unlock this fd */
	lockPath := path.Join(lockDir, lockFile)
	fd, err := syscall.Open(lockPath, syscall.O_CREAT, syscall.S_IWUSR|syscall.S_IRUSR)
	if err != nil {
		glog.Errorf("Unable to open lock file %v", err)
		return err
	}

	syscall.CloseOnExec(fd)

	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		glog.Error("Launcher is already running.  Exitting.")
		return fmt.Errorf("Unable to lock file %s", lockPath)
	}

	return nil
}

/* Must be called after flag.Parse() */
func initLogger() error {
	logDirFlag := flag.Lookup("log_dir")
	if logDirFlag == nil {
		return fmt.Errorf("log_dir does not exist")
	}

	if logDirFlag.Value.String() == "" {
		if err := logDirFlag.Value.Set(logDir); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(logDirFlag.Value.String(), 0755); err != nil {
		return fmt.Errorf("Unable to create log directory (%s) %v", logDir, err)
	}

	return nil
}

func createMandatoryDirs() error {
	if err := os.MkdirAll(instancesDir, 0755); err != nil {
		return fmt.Errorf("Unable to create instances directory (%s) %v",
			instancesDir, err)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("Unable to create data directory (%s) %v", dataDir, err)
	}

	return nil
}

func setLimits() {
	var rlim syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlim)
	if err != nil {
		glog.Warningf("Getrlimit failed %v", err)
		return
	}

	glog.Infof("Initial nofile limits: cur %d max %d", rlim.Cur, rlim.Max)

	if rlim.Cur < rlim.Max {
		oldCur := rlim.Cur
		rlim.Cur = rlim.Max
		err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlim)
		if err != nil {
			glog.Warningf("Setrlimit failed %v", err)
			rlim.Cur = oldCur
		}
	}

	glog.Infof("Updated nofile limits: cur %d max %d", rlim.Cur, rlim.Max)

	maxInstances = int(rlim.Cur / 5)
}

func startLauncher() int {
	doneCh := make(chan struct{})
	statusCh := make(chan struct{})
	signalCh := make(chan os.Signal, 1)
	timeoutCh := make(chan struct{})
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go connectToServer(doneCh, statusCh)

DONE:
	for {
		select {
		case <-signalCh:
			glog.Info("Received terminating signal.  Waiting for server loop to quit")
			close(doneCh)
			go func() {
				time.Sleep(time.Second)
				timeoutCh <- struct{}{}
			}()
		case <-statusCh:
			glog.Info("Server Loop quit cleanly")
			break DONE
		case <-timeoutCh:
			glog.Warning("Server Loop did not exit within 1 second quitting")
			glog.Flush()

			/* We panic here to see which naughty go routines are still running. */
			debug.SetTraceback("all")
			panic("Server Loop did not exit within 1 second quitting")
		}
	}

	return 0
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("%s", debug.Stack())
			glog.Flush()
		}
	}()

	flag.Parse()

	if simulate == false && getLock() != nil {
		os.Exit(1)
	}

	libsnnet.Logger = gloginterface.CiaoGlogLogger{}

	if err := initLogger(); err != nil {
		log.Fatalf("Unable to initialise logs: %v", err)
	}

	glog.Info("Starting Launcher")

	exitCode := 0
	var stopProfile func()
	if profileFN != nil {
		stopProfile = profileFN()
	}

	var stopTrace func()
	if traceFN != nil {
		stopTrace = traceFN()
	}

	if hardReset {
		purgeLauncherState()
	} else {
		setLimits()

		glog.Infof("Launcher will allow a maximum of %d instances", maxInstances)

		if err := createMandatoryDirs(); err != nil {
			glog.Fatalf("Unable to create mandatory dirs: %v", err)
		}

		exitCode = startLauncher()
	}

	if stopTrace != nil {
		stopTrace()
	}

	if stopProfile != nil {
		stopProfile()
	}

	glog.Flush()
	glog.Info("Exit")

	os.Exit(exitCode)
}
