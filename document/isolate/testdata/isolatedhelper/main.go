package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

var (
	mode           = "echo"
	networkAddress string
)

type echoResponse struct {
	Executable   string            `json:"executable"`
	PID          int               `json:"pid"`
	Status       string            `json:"status"`
	ProcReadOnly bool              `json:"proc_read_only"`
	Namespaces   map[string]string `json:"namespaces"`
	Arguments    []string          `json:"arguments"`
	Environment  []string          `json:"environment"`
	StdinSHA256  string            `json:"stdin_sha256"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--descendant" {
		lock, err := os.OpenFile(networkAddress, os.O_RDWR|os.O_TRUNC, 0)
		if err != nil || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			os.Exit(5)
		}
		if _, err := fmt.Fprintln(lock, "descendant-ready"); err != nil || lock.Sync() != nil {
			os.Exit(6)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	if len(os.Args) != 3 || os.Args[1] != "--protocol" || (os.Args[2] != "docbank-trafilatura/v2" && os.Args[2] != "docbank-pymupdf/v1") {
		os.Exit(8)
	}
	switch mode {
	case "echo", "replacement":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			os.Exit(9)
		}
		var proc unix.Statfs_t
		if unix.Statfs("/proc", &proc) != nil {
			os.Exit(10)
		}
		namespaces := map[string]string{}
		for _, name := range []string{"user", "net", "pid", "mnt"} {
			namespaces[name], err = os.Readlink("/proc/self/ns/" + name)
			if err != nil {
				os.Exit(11)
			}
		}
		var controls []string
		for line := range strings.SplitSeq(string(status), "\n") {
			if strings.HasPrefix(line, "NoNewPrivs:") || strings.HasPrefix(line, "Seccomp:") {
				controls = append(controls, line)
			}
		}
		digest := sha256.Sum256(data)
		_ = json.NewEncoder(os.Stdout).Encode(echoResponse{
			Executable: os.Args[0], PID: os.Getpid(), Status: strings.Join(controls, "\n"), ProcReadOnly: proc.Flags&unix.ST_RDONLY != 0, Namespaces: namespaces,
			Arguments: os.Args[1:], Environment: os.Environ(),
			StdinSHA256: hex.EncodeToString(digest[:]),
		})
	case "network":
		connection, err := net.DialTimeout("tcp", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "unix-network":
		connection, err := net.DialTimeout("unix", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "descendant":
		command := exec.Command(os.Args[0], "--descendant")
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Print("spawned")
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour)
		}
	case "overflow":
		block := make([]byte, 32<<10)
		for {
			if _, err := os.Stdout.Write(block); err != nil {
				return
			}
		}
	case "exit-125":
		os.Exit(125)
	default:
		os.Exit(4)
	}
}
