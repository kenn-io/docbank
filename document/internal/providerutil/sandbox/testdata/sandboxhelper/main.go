package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	mode           = "echo"
	networkAddress string
	outputName     = "output.bin"
)

type echoResponse struct {
	Arguments   []string `json:"arguments"`
	Environment []string `json:"environment"`
	StdinSHA256 string   `json:"stdin_sha256"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--descendant" {
		if mode == "file-descendant-exit" {
			if err := os.WriteFile("/work/descendant-ready", []byte("ready"), 0o600); err != nil {
				os.Exit(6)
			}
			for {
				time.Sleep(time.Hour)
			}
		}
		if _, err := fmt.Fprintln(os.Stdout, "descendant-ready"); err != nil || os.Stdout.Sync() != nil {
			os.Exit(6)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("DOCBANK_SANDBOX_WARMUP") == "1" {
		if err := runWarmup(); err != nil {
			os.Exit(6)
		}
		return
	}

	switch mode {
	case "ipc", "file-ipc":
		id, err := strconv.Atoi(networkAddress)
		if err != nil {
			os.Exit(2)
		}
		host := "denied"
		memory, err := unix.SysvShmAttach(id, 0, unix.SHM_RDONLY)
		if err == nil {
			host = string(memory)
			if err := unix.SysvShmDetach(memory); err != nil {
				os.Exit(2)
			}
		} else if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EIDRM) {
			fmt.Fprint(os.Stderr, err)
			os.Exit(2)
		}
		privateID, err := unix.SysvShmGet(unix.IPC_PRIVATE, 1, unix.IPC_CREAT|0o600)
		if err != nil {
			os.Exit(2)
		}
		private, err := unix.SysvShmAttach(privateID, 0, 0)
		if err == nil {
			private[0] = 1
			err = unix.SysvShmDetach(private)
		}
		_, removeErr := unix.SysvShmCtl(privateID, unix.IPC_RMID, nil)
		if err := errors.Join(err, removeErr); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(2)
		}
		status := "host=" + host + ";private=allowed"
		if mode == "ipc" {
			fmt.Print(status)
		} else if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(2)
		}
	case "exec-descriptor":
		target, _ := os.Readlink("/proc/self/fd/3")
		fmt.Print(target)
	case "echo", "replacement":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		digest := sha256.Sum256(data)
		_ = json.NewEncoder(os.Stdout).Encode(echoResponse{
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
	case "strict-fs":
		probePath := "/tmp/docbank-strict-probe"
		writeErr := os.WriteFile(probePath, []byte("temp"), 0o600)
		data, readErr := os.ReadFile(probePath)
		removeErr := os.Remove(probePath)
		comm, commErr := os.ReadFile("/proc/1/comm")
		privateProc := commErr == nil && !strings.Contains(strings.ToLower(string(comm)), "init")
		tmpAllowed := writeErr == nil && readErr == nil && string(data) == "temp" && removeErr == nil
		fmt.Printf("proc=%t;tmp=%t;comm=%s;write=%v;read=%v;remove=%v", privateProc, tmpAllowed, strings.TrimSpace(string(comm)), writeErr, readErr, removeErr)
	case "file-network":
		status := "denied"
		connection, err := net.DialTimeout("tcp", networkAddress, time.Second)
		if err == nil {
			status = "connected"
			_ = connection.Close()
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-unix-network":
		status := "denied"
		connection, err := net.DialTimeout("unix", networkAddress, time.Second)
		if err == nil {
			status = "connected"
			_ = connection.Close()
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "local-ipc":
		for _, domain := range []int{unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK, unix.AF_VSOCK, -1} {
			if _, err := unix.Socket(domain, unix.SOCK_STREAM, 0); !errors.Is(err, unix.EPERM) {
				fmt.Fprintf(os.Stderr, "socket domain %d: %v", domain, err)
				os.Exit(7)
			}
			if _, err := unix.Socketpair(domain, unix.SOCK_STREAM, 0); !errors.Is(err, unix.EPERM) {
				fmt.Fprintf(os.Stderr, "socketpair domain %d: %v", domain, err)
				os.Exit(7)
			}
		}
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			os.Exit(7)
		}
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
		if err := os.WriteFile("/work/"+outputName, []byte("allowed"), 0o600); err != nil {
			os.Exit(5)
		}
	case "unix-network":
		connection, err := net.DialTimeout("unix", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "host-file":
		_, readErr := os.ReadFile(networkAddress)
		writeErr := os.WriteFile(networkAddress, []byte("modified"), 0o600)
		chmodErr := os.Chmod(networkAddress, 0o644)
		changedTime := time.Unix(1, 0)
		timesErr := os.Chtimes(networkAddress, changedTime, changedTime)
		status := "exposed"
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			status = "denied"
		}
		fmt.Print(status)
	case "file-host":
		_, readErr := os.ReadFile(networkAddress)
		writeErr := os.WriteFile(networkAddress, []byte("modified"), 0o600)
		chmodErr := os.Chmod(networkAddress, 0o644)
		changedTime := time.Unix(1, 0)
		timesErr := os.Chtimes(networkAddress, changedTime, changedTime)
		status := "exposed"
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			status = "denied"
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "descendant":
		command := exec.Command("/proc/self/exe", "--descendant")
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
	case "file-output":
		if err := os.WriteFile("/work/"+outputName, []byte("supervised output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-overflow":
		if err := os.WriteFile("/work/"+outputName, make([]byte, 2048), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-descendant-exit":
		command := exec.Command("/proc/self/exe", "--descendant")
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		if err := waitForFile("/work/descendant-ready"); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile("/work/"+outputName, []byte("supervised output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "caller-exit81-hidden":
		if err := verifyCallerBoundary(); err != nil {
			os.Exit(6)
		}
		if _, err := os.Stat("/work/profile/caller-launch"); err == nil {
			if err := os.WriteFile("/work/"+outputName, []byte("unexpected caller retry"), 0o600); err != nil {
				os.Exit(5)
			}
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			os.Exit(6)
		}
		if err := os.WriteFile("/work/profile/caller-launch", []byte("one"), 0o600); err != nil {
			os.Exit(5)
		}
		if err := os.WriteFile("/work/.caller-hidden", []byte("hidden"), 0o600); err != nil {
			os.Exit(5)
		}
		os.Exit(81)
	case "warmup-no-output", "warmup-cleanup-failure":
		if err := os.WriteFile("/work/"+outputName, []byte("caller output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "warmup-contract", "warmup-budget", "warmup-input-mutation", "warmup-second81", "warmup-non81":
		if mode == "warmup-budget" {
			if err := os.WriteFile("/work/profile/caller-launch", []byte("one"), 0o600); err != nil {
				os.Exit(5)
			}
			if err := os.WriteFile("/work/"+outputName, make([]byte, 12<<20), 0o600); err != nil {
				os.Exit(5)
			}
			return
		}
		if mode != "warmup-second81" && mode != "warmup-non81" {
			if err := verifyCallerBoundary(); err != nil {
				os.Exit(6)
			}
		}
		if err := os.WriteFile("/work/profile/caller-launch", []byte("one"), 0o600); err != nil {
			os.Exit(5)
		}
		if err := os.WriteFile("/work/"+outputName, []byte("caller output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "argv":
		if err := os.WriteFile("/work/"+outputName, []byte(strings.Join(os.Args[1:], "\x00")), 0o600); err != nil {
			os.Exit(5)
		}
	case "runtime-mounts":
		if len(os.Args) < 4 {
			os.Exit(6)
		}
		statuses := make([]string, 0, len(os.Args)-2)
		for _, path := range os.Args[2:] {
			var stat unix.Statfs_t
			if err := unix.Statfs(path, &stat); err != nil {
				os.Exit(7)
			}
			statuses = append(statuses, filepath.Base(path)+"="+strconv.FormatBool(stat.Flags&unix.ST_NOEXEC == 0))
		}
		execBytes, err := os.ReadFile("/usr/runtime/exec")
		if err != nil {
			os.Exit(8)
		}
		dataBytes, err := os.ReadFile("/usr/runtime/data")
		if err != nil {
			os.Exit(8)
		}
		payload := strings.Join(statuses, ",") + ";exec-content=" + string(execBytes) + ";data-content=" + string(dataBytes)
		if err := os.WriteFile("/work/"+outputName, []byte(payload), 0o600); err != nil {
			os.Exit(5)
		}
	case "fd-count":
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			os.Exit(9)
		}
		if err := os.WriteFile("/work/"+outputName, []byte(strconv.Itoa(len(entries))), 0o600); err != nil {
			os.Exit(5)
		}
	case "exit-125":
		os.Exit(125)
	case "exit-124":
		os.Exit(124)
	default:
		os.Exit(4)
	}
}

const expectedProfileSettings = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry">
 <item oor:path="/org.openoffice.Office.Common/Security/Scripting">
  <prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>
  <prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop>
  <prop oor:name="DisableActiveContent" oor:op="fuse"><value>true</value></prop>
  <prop oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value></prop>
 </item>
</oor:items>`

func runWarmup() error {
	input, err := os.ReadFile("/work/source.docx")
	if err != nil {
		return fmt.Errorf("read warm-up input: %w", err)
	}
	if mode == "warmup-non81" {
		if !bytes.Equal(input, []byte("warmup")) {
			return fmt.Errorf("unexpected trusted input: %q", input)
		}
		if err := writeWarmupState(); err != nil {
			return err
		}
		if err := os.WriteFile("/work/"+outputName, []byte("warm-up output"), 0o600); err != nil {
			return err
		}
		os.Exit(82)
	}
	if mode == "warmup-second81" {
		if !bytes.Equal(input, []byte("warmup")) {
			return fmt.Errorf("unexpected trusted input: %q", input)
		}
		marker, err := os.ReadFile("/work/profile/warmup-launches")
		if errors.Is(err, os.ErrNotExist) {
			if err := writeWarmupState(); err != nil {
				return err
			}
			if err := os.WriteFile("/work/profile/warmup-launches", []byte("one"), 0o600); err != nil {
				return err
			}
		} else if err != nil || string(marker) != "one" {
			return fmt.Errorf("warm-up launch marker: %v", err)
		} else if err := os.WriteFile("/work/profile/warmup-launches", []byte("two"), 0o600); err != nil {
			return err
		} else if err := os.WriteFile("/work/"+outputName, []byte("warm-up output"), 0o600); err != nil {
			return err
		}
		os.Exit(81)
	}
	if mode == "warmup-no-output" {
		return nil
	}
	if mode == "warmup-overflow" {
		return os.WriteFile("/work/"+outputName, make([]byte, 2048), 0o600)
	}
	marker, err := os.ReadFile("/work/profile/warmup-marker")
	if errors.Is(err, os.ErrNotExist) {
		if !bytes.Equal(input, []byte("warmup")) {
			return fmt.Errorf("unexpected trusted input: %q", input)
		}
		if err := writeWarmupState(); err != nil {
			return err
		}
		if mode == "warmup-input-mutation" {
			if err := os.WriteFile("/work/source.docx", []byte("tampered"), 0o600); err != nil {
				return err
			}
		}
		os.Exit(81)
	}
	if err != nil || string(marker) != "trusted" {
		return fmt.Errorf("trusted profile marker: %v", err)
	}
	if mode != "warmup-input-mutation" && !bytes.Equal(input, []byte("warmup")) {
		return fmt.Errorf("unexpected trusted input: %q", input)
	}
	if mode == "warmup-input-mutation" && bytes.Equal(input, []byte("warmup")) {
		return errors.New("warm-up input mutation was not observed")
	}
	if _, err := os.Lstat("/work/" + outputName); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stale warm-up output remains: %v", err)
	}
	if err := os.WriteFile("/work/"+outputName, []byte("warm-up output"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile("/work/profile/warmup-launches", []byte("two"), 0o600); err != nil {
		return err
	}
	if mode == "warmup-budget" {
		if err := os.WriteFile("/work/warmup-fill", make([]byte, 56<<20), 0o600); err != nil {
			return err
		}
	}
	if mode == "warmup-contract" {
		conflict := `<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>false</value></prop></item>`
		settings := strings.Replace(expectedProfileSettings, "</oor:items>", conflict+"</oor:items>", 1)
		return os.WriteFile("/work/profile/user/registrymodifications.xcu", []byte(settings), 0o600)
	}
	if mode == "warmup-cleanup-failure" {
		if err := os.RemoveAll("/work/home"); err != nil {
			return err
		}
		return os.WriteFile("/work/home", []byte("not a directory"), 0o600)
	}
	return nil
}

func writeWarmupState() error {
	for path, data := range map[string][]byte{
		"/work/profile/warmup-marker":   []byte("trusted"),
		"/work/profile/warmup-launches": []byte("one"),
		"/work/.warmup-root":            []byte("root"),
		"/work/home/warmup-home":        []byte("home"),
		"/work/home/cache/warmup-cache": []byte("cache"),
		"/work/out/warmup-out":          []byte("out"),
		"/work/tmp/warmup-tmp":          []byte("tmp"),
		"/work/" + outputName:           []byte("stale warm-up output"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func verifyCallerBoundary() error {
	marker, err := os.ReadFile("/work/profile/warmup-marker")
	if err != nil || string(marker) != "trusted" {
		return fmt.Errorf("warm-up profile marker: %v", err)
	}
	launches, err := os.ReadFile("/work/profile/warmup-launches")
	if err != nil || string(launches) != "two" {
		return fmt.Errorf("warm-up launch count: %v", err)
	}
	settings, err := os.ReadFile("/work/profile/user/registrymodifications.xcu")
	if err != nil || string(settings) != expectedProfileSettings {
		return fmt.Errorf("profile security settings: %v", err)
	}
	input, err := os.ReadFile("/work/source.docx")
	if err != nil || !bytes.Equal(input, []byte("input")) {
		return fmt.Errorf("caller input: %v", err)
	}
	if _, err := os.Lstat("/work/" + outputName); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("caller output was preexisting: %v", err)
	}
	for _, path := range []string{
		"/work/.warmup-root", "/work/home/warmup-home", "/work/home/cache/warmup-cache",
		"/work/out/warmup-out", "/work/tmp/warmup-tmp",
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("warm-up sentinel remains at %s: %v", path, err)
		}
	}
	return nil
}

func waitForFile(path string) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}
