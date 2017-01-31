package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/rundmc/dadoo"

	"github.com/eapache/go-resiliency/retrier"
	"github.com/opencontainers/runc/libcontainer/system"

	cmsg "github.com/opencontainers/runc/libcontainer/utils"
)

const MaxSocketDirPathLength = 80

var (
	uid           = flag.Int("uid", 0, "uid to chown console to")
	gid           = flag.Int("gid", 0, "gid to chown console to")
	tty           = flag.Bool("tty", false, "tty requested")
	socketDirPath = flag.String("socket-dir-path", "", "path to a dir in which to store console sockets")

	ioWg *sync.WaitGroup = &sync.WaitGroup{}
)

func main() {
	os.Exit(run())
}

func run() int {
	flag.Parse()

	runtime := flag.Args()[1]         // e.g. runc
	processStateDir := flag.Args()[2] // path to a dir in which to store process state (e.g. fifos)
	containerId := flag.Args()[3]

	signals := make(chan os.Signal, 100)
	signal.Notify(signals, syscall.SIGCHLD)

	fd3 := os.NewFile(3, "/proc/self/fd/3")
	logFile := fmt.Sprintf("/proc/%d/fd/4", os.Getpid())
	logFD := os.NewFile(4, "/proc/self/fd/4")
	syncPipe := os.NewFile(5, "/proc/self/fd/5")
	pidFilePath := filepath.Join(processStateDir, "pidfile")

	stdin, stdout, stderr, winsz := openPipes(processStateDir)

	syncPipe.Write([]byte{0})

	var runcExecCmd *exec.Cmd
	if *tty {
		if len(*socketDirPath) > MaxSocketDirPathLength {
			panic(fmt.Sprintf("value for --socket-dir-path cannot exceed %d characters in length", MaxSocketDirPathLength))
		}
		ttySocketPath := setupTTYSocket(stdin, stdout, winsz, pidFilePath, *socketDirPath)
		runcExecCmd = exec.Command(runtime, "-debug", "-log", logFile, "exec", "-d", "-tty", "-console-socket", ttySocketPath, "-p", fmt.Sprintf("/proc/%d/fd/0", os.Getpid()), "-pid-file", pidFilePath, containerId)
	} else {
		runcExecCmd = exec.Command(runtime, "-debug", "-log", logFile, "exec", "-p", fmt.Sprintf("/proc/%d/fd/0", os.Getpid()), "-d", "-pid-file", pidFilePath, containerId)
		runcExecCmd.Stdin = stdin
		runcExecCmd.Stdout = stdout
		runcExecCmd.Stderr = stderr
	}

	// we need to be the subreaper so we can wait on the detached container process
	system.SetSubreaper(os.Getpid())

	if err := runcExecCmd.Start(); err != nil {
		fd3.Write([]byte{2})
		return 2
	}

	var status syscall.WaitStatus
	var rusage syscall.Rusage
	_, err := syscall.Wait4(runcExecCmd.Process.Pid, &status, 0, &rusage)
	check(err)    // Start succeeded but Wait4 failed, this can only be a programmer error
	logFD.Close() // No more logs from runc so close fd

	// also check that masterFD is received and streaming or whatevs
	fd3.Write([]byte{byte(status.ExitStatus())})
	if status.ExitStatus() != 0 {
		return 3 // nothing to wait for, container didn't launch
	}

	containerPid, err := parsePid(pidFilePath)
	check(err)

	return waitForContainerToExit(processStateDir, containerPid, signals)
}

func waitForContainerToExit(processStateDir string, containerPid int, signals chan os.Signal) (exitCode int) {
	for range signals {
		for {
			var status syscall.WaitStatus
			var rusage syscall.Rusage
			wpid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, &rusage)
			if err != nil || wpid <= 0 {
				break // wait for next SIGCHLD
			}

			if wpid == containerPid {
				exitCode = status.ExitStatus()
				if status.Signaled() {
					exitCode = 128 + int(status.Signal())
				}

				ioWg.Wait() // wait for full output to be collected

				check(ioutil.WriteFile(filepath.Join(processStateDir, "exitcode"), []byte(strconv.Itoa(exitCode)), 0700))
				return exitCode
			}
		}
	}

	panic("ran out of signals") // cant happen
}

func openPipes(processStateDir string) (io.Reader, io.Writer, io.Writer, io.Reader) {
	stdin := openFifo(filepath.Join(processStateDir, "stdin"), os.O_RDONLY)
	stdout := openFifo(filepath.Join(processStateDir, "stdout"), os.O_WRONLY|os.O_APPEND)
	stderr := openFifo(filepath.Join(processStateDir, "stderr"), os.O_WRONLY|os.O_APPEND)
	winsz := openFifo(filepath.Join(processStateDir, "winsz"), os.O_RDWR)
	openFifo(filepath.Join(processStateDir, "exit"), os.O_RDWR) // open just so guardian can detect it being closed when we exit

	return stdin, stdout, stderr, winsz
}

func openFifo(path string, flags int) io.ReadWriter {
	r, err := os.OpenFile(path, flags, 0600)
	if os.IsNotExist(err) {
		return nil
	}

	check(err)
	return r
}

func setupTTYSocket(stdin io.Reader, stdout io.Writer, winszFifo io.Reader, pidFilePath, sockDirBase string) string {
	sockDir, err := ioutil.TempDir(sockDirBase, "")
	check(err)

	ttySockPath := filepath.Join(sockDir, "tty.sock")
	l, err := net.Listen("unix", ttySockPath)
	check(err)

	//go to the background and set master
	go func(ln net.Listener) (err error) {
		// if any of the following errors, it means runc has connected to the
		// socket, so it must've started, thus we might need to kill the process
		defer func() {
			if err != nil {
				killProcess(pidFilePath)
				panic(err)
			}
		}()

		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Close ln, to allow for other instances to take over.
		ln.Close()

		// Get the fd of the connection.
		unixconn, ok := conn.(*net.UnixConn)
		if !ok {
			return
		}

		socket, err := unixconn.File()
		if err != nil {
			return
		}
		defer socket.Close()

		// Get the master file descriptor from runC.
		master, err := cmsg.RecvFd(socket)
		if err != nil {
			return
		}

		os.RemoveAll(sockDir)
		streamProcess(master, stdin, stdout, winszFifo)

		return
	}(l)

	return ttySockPath
}

func streamProcess(m *os.File, stdin io.Reader, stdout io.Writer, winszFifo io.Reader) {
	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		io.Copy(stdout, m)
	}()

	go io.Copy(m, stdin)

	go func() {
		for {
			var winSize garden.WindowSize
			if err := json.NewDecoder(winszFifo).Decode(&winSize); err != nil {
				println("invalid winsz event", err)
				continue // not much we can do here..
			}
			dadoo.SetWinSize(m, winSize)
		}
	}()
}

func killProcess(pidFilePath string) {
	pid, err := readPid(pidFilePath)
	if err == nil {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func readPid(pidFilePath string) (int, error) {
	retrier := retrier.New(retrier.ConstantBackoff(20, 500*time.Millisecond), nil)
	var (
		pid int = -1
		err error
	)
	retrier.Run(func() error {
		pid, err = parsePid(pidFilePath)
		return err
	})

	return pid, err
}

func parsePid(pidFile string) (int, error) {
	b, err := ioutil.ReadFile(pidFile)
	if err != nil {
		return -1, err
	}

	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil {
		return -1, err
	}

	return pid, nil
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
