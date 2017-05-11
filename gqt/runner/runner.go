package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"code.cloudfoundry.org/garden/client"
	"code.cloudfoundry.org/garden/client/connection"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/gomega/gbytes"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

const MNT_DETACH = 0x2

var DataDir string
var TarBin = os.Getenv("GARDEN_TAR_PATH")

type RunningGarden struct {
	client.Client

	runner  GardenRunner
	process ifrit.Process

	debugIP   string
	debugPort int

	Pid int

	Tmpdir string

	DepotDir  string
	DataDir   string
	GraphPath string

	logger lager.Logger
}

type GardenRunner struct {
	*ginkgomon.Runner
	Cmd            *exec.Cmd
	TmpDir         string
	GraphPath      string
	ConsoleSockets string
	DepotDir       string
	DebugIp        string
	DebugPort      int
	Network, Addr  string
}

func init() {
	DataDir = os.Getenv("GARDEN_TEST_GRAPHPATH")
	if DataDir == "" {
		// This must be set outside of the Ginkgo node directory (tmpDir) because
		// otherwise the Concourse worker may run into one of the AUFS kernel
		// module bugs that cause the VM to become unresponsive.
		DataDir = "/tmp/aufs_mount"
	}
}

func NewGardenRunner(bin, initBin, nstarBin, dadooBin, grootfsBin, rootfs, tarBin, network, address string, user UserCredential, argv ...string) GardenRunner {
	r := GardenRunner{}

	r.Network = network
	r.Addr = address
	r.TmpDir = filepath.Join(
		os.TempDir(),
		fmt.Sprintf("test-garden-%d", ginkgo.GinkgoParallelNode()),
	)

	r.GraphPath = filepath.Join(DataDir, fmt.Sprintf("node-%d", ginkgo.GinkgoParallelNode()))
	r.DepotDir = filepath.Join(r.TmpDir, "containers")
	r.ConsoleSockets = filepath.Join(r.TmpDir, "console-sockets")

	MustMountTmpfs(r.GraphPath)

	r.Cmd = cmd(r.TmpDir, r.DepotDir, r.GraphPath, r.ConsoleSockets, r.Network, r.Addr, bin, initBin, nstarBin, dadooBin, grootfsBin, tarBin, rootfs, user, argv...)
	r.Cmd.Env = append(os.Environ(), fmt.Sprintf("TMPDIR=%s", r.TmpDir))

	for i, arg := range r.Cmd.Args {
		if arg == "--debug-bind-ip" {
			r.DebugIp = r.Cmd.Args[i+1]
		}
		if arg == "--debug-bind-port" {
			r.DebugPort, _ = strconv.Atoi(r.Cmd.Args[i+1])
		}
	}

	r.Runner = ginkgomon.New(ginkgomon.Config{
		Name:              "guardian",
		Command:           r.Cmd,
		AnsiColorCode:     "31m",
		StartCheck:        "guardian.started",
		StartCheckTimeout: 30 * time.Second,
	})

	return r
}

func Start(bin, initBin, nstarBin, dadooBin, grootfsBin, rootfs, tarBin string, user UserCredential, argv ...string) *RunningGarden {
	runner := NewGardenRunner(bin, initBin, nstarBin, dadooBin, grootfsBin, rootfs, tarBin, "unix", fmt.Sprintf("/tmp/garden_%d.sock", GinkgoParallelNode()), user, argv...)

	r := &RunningGarden{
		runner:   runner,
		DepotDir: runner.DepotDir,

		DataDir:   DataDir,
		GraphPath: runner.GraphPath,
		Tmpdir:    runner.TmpDir,
		logger:    lagertest.NewTestLogger("garden-runner"),

		debugIP:   runner.DebugIp,
		debugPort: runner.DebugPort,

		Client: client.New(connection.New(runner.Network, runner.Addr)),
	}

	r.process = ifrit.Invoke(r.runner)
	r.Pid = runner.Cmd.Process.Pid

	return r
}

func (r *RunningGarden) Kill() error {
	r.process.Signal(syscall.SIGKILL)
	select {
	case err := <-r.process.Wait():
		return err
	case <-time.After(time.Second * 10):
		r.process.Signal(syscall.SIGKILL)
		return errors.New("timed out waiting for garden to shutdown after 10 seconds")
	}
}

func (r *RunningGarden) DestroyAndStop() error {
	if err := r.DestroyContainers(); err != nil {
		return err
	}

	if err := r.Stop(); err != nil {
		return err
	}

	return nil
}

func (r *RunningGarden) Stop() error {
	r.process.Signal(syscall.SIGTERM)

	var err error
	for i := 0; i < 5; i++ {
		select {
		case err := <-r.process.Wait():
			return err
		case <-time.After(time.Second * 5):
			r.process.Signal(syscall.SIGTERM)
			err = errors.New("timed out waiting for garden to shutdown after 5 seconds")
		}
	}

	r.process.Signal(syscall.SIGKILL)
	return err
}

func (r *RunningGarden) DestroyContainers() error {
	containers, err := r.Containers(nil)
	if err != nil {
		return err
	}

	for _, container := range containers {
		r.Destroy(container.Handle())
	}

	return nil
}

type debugVars struct {
	NumGoRoutines int `json:"numGoRoutines"`
}

func (r *RunningGarden) DumpGoroutines() (string, error) {
	debugURL := fmt.Sprintf("http://%s:%d/debug/pprof/goroutine?debug=2", r.debugIP, r.debugPort)
	res, err := http.Get(debugURL)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	b, err := ioutil.ReadAll(res.Body)
	return string(b), err
}

func (r *RunningGarden) NumGoroutines() (int, error) {
	debugURL := fmt.Sprintf("http://%s:%d/debug/vars", r.debugIP, r.debugPort)
	res, err := http.Get(debugURL)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	decoder := json.NewDecoder(res.Body)
	var debugVarsData debugVars
	err = decoder.Decode(&debugVarsData)
	if err != nil {
		return 0, err
	}

	return debugVarsData.NumGoRoutines, nil
}

func (r *RunningGarden) Buffer() *gbytes.Buffer {
	return r.runner.Buffer()
}

func (r *RunningGarden) ExitCode() int {
	return r.runner.ExitCode()
}
