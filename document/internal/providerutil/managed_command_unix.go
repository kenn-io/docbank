//go:build darwin || linux

package providerutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"
)

const descendantPollInterval = 25 * time.Millisecond

type processIdentity struct {
	pid       int
	startedAt uint64
}

type processRecord struct {
	identity processIdentity
	parentID int
}

type managedProcessTree struct {
	processGroupID int
	root           processIdentity
	descendants    map[processIdentity]*os.Process
	trackerStop    chan struct{}
	trackerDone    chan struct{}
	trackerErr     error
	tracking       bool
}

func newManagedProcessTree(command *exec.Cmd) (*managedProcessTree, error) {
	attributes := &syscall.SysProcAttr{}
	if command.SysProcAttr != nil {
		copied := *command.SysProcAttr
		attributes = &copied
	}
	attributes.Setpgid = true
	attributes.Pgid = 0
	command.SysProcAttr = attributes
	return &managedProcessTree{}, nil
}

func (tree *managedProcessTree) attach(process *os.Process) error {
	tree.processGroupID = process.Pid
	tree.root = processIdentity{pid: process.Pid}
	tree.descendants = make(map[processIdentity]*os.Process)
	records, err := snapshotProcessTable()
	if err != nil {
		return fmt.Errorf("read process table: %w", err)
	}
	tree.recordDescendants(records)
	tree.trackerStop = make(chan struct{})
	tree.trackerDone = make(chan struct{})
	tree.tracking = true
	go tree.trackDescendants()
	return nil
}

func (tree *managedProcessTree) kill() error {
	if tree.processGroupID <= 0 {
		return os.ErrProcessDone
	}
	tree.stopTracking()
	groupStopErr := signalProcessGroup(tree.processGroupID, syscall.SIGSTOP)
	if errors.Is(groupStopErr, os.ErrProcessDone) && len(tree.descendants) == 0 {
		if tree.trackerErr != nil {
			return tree.trackerErr
		}
		return os.ErrProcessDone
	}
	if errors.Is(groupStopErr, os.ErrProcessDone) {
		groupStopErr = nil
	}
	freezeErr := errors.Join(groupStopErr, tree.freezeDescendants())
	descendantsKilled, descendantErr := tree.signalDescendants(os.Kill)
	groupErr := signalProcessGroup(tree.processGroupID, syscall.SIGKILL)
	if errors.Is(groupErr, os.ErrProcessDone) {
		groupErr = nil
	}
	result := errors.Join(tree.trackerErr, freezeErr, descendantErr, groupErr)
	if result != nil {
		return result
	}
	if !descendantsKilled && groupErr == nil {
		if err := syscall.Kill(-tree.processGroupID, 0); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
	}
	return nil
}

func (tree *managedProcessTree) close() error {
	killErr := tree.kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	var releaseErr error
	for identity, process := range tree.descendants {
		if err := process.Release(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			releaseErr = errors.Join(releaseErr,
				fmt.Errorf("release tracked process %d: %w", identity.pid, err))
		}
		delete(tree.descendants, identity)
	}
	return errors.Join(killErr, releaseErr)
}

func (tree *managedProcessTree) trackDescendants() {
	defer close(tree.trackerDone)
	ticker := time.NewTicker(descendantPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-tree.trackerStop:
			return
		case <-ticker.C:
			records, err := snapshotProcessTable()
			if err != nil {
				if tree.trackerErr == nil {
					tree.trackerErr = fmt.Errorf("track descendant processes: %w", err)
				}
				continue
			}
			tree.recordDescendants(records)
		}
	}
}

func (tree *managedProcessTree) stopTracking() {
	if !tree.tracking {
		return
	}
	close(tree.trackerStop)
	<-tree.trackerDone
	tree.tracking = false
}

func (tree *managedProcessTree) recordDescendants(records []processRecord) int {
	current := make(map[int]processIdentity, len(records))
	children := make(map[int][]processRecord)
	for _, record := range records {
		current[record.identity.pid] = record.identity
		children[record.parentID] = append(children[record.parentID], record)
		if record.identity.pid == tree.root.pid && tree.root.startedAt == 0 {
			tree.root = record.identity
		}
	}
	seeds := []int{}
	if identity, exists := current[tree.root.pid]; exists &&
		(tree.root.startedAt == 0 || identity == tree.root) {
		seeds = append(seeds, tree.root.pid)
	}
	for identity := range tree.descendants {
		if current[identity.pid] == identity {
			seeds = append(seeds, identity.pid)
		}
	}
	added := 0
	seen := make(map[int]bool, len(seeds))
	for len(seeds) > 0 {
		parentID := seeds[0]
		seeds = seeds[1:]
		if seen[parentID] {
			continue
		}
		seen[parentID] = true
		for _, child := range children[parentID] {
			seeds = append(seeds, child.identity.pid)
			if child.identity == tree.root {
				continue
			}
			if _, exists := tree.descendants[child.identity]; exists {
				continue
			}
			process, err := os.FindProcess(child.identity.pid)
			if err != nil {
				continue
			}
			tree.descendants[child.identity] = process
			added++
		}
	}
	return added
}

func (tree *managedProcessTree) freezeDescendants() error {
	var result error
	for {
		if _, err := tree.signalDescendants(syscall.SIGSTOP); err != nil {
			result = errors.Join(result, err)
		}
		records, err := snapshotProcessTable()
		if err != nil {
			return errors.Join(result, fmt.Errorf("freeze descendant processes: %w", err))
		}
		if tree.recordDescendants(records) == 0 {
			return result
		}
	}
}

func (tree *managedProcessTree) signalDescendants(signal os.Signal) (bool, error) {
	records, err := snapshotProcessTable()
	if err != nil {
		return false, fmt.Errorf("verify descendant processes: %w", err)
	}
	current := make(map[processIdentity]bool, len(records))
	for _, record := range records {
		current[record.identity] = true
	}
	identities := make([]processIdentity, 0, len(tree.descendants))
	for identity := range tree.descendants {
		identities = append(identities, identity)
	}
	slices.SortFunc(identities, func(left, right processIdentity) int { return right.pid - left.pid })
	signaled := false
	var result error
	for _, identity := range identities {
		if !current[identity] {
			continue
		}
		if err := tree.descendants[identity].Signal(signal); err != nil {
			if !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				result = errors.Join(result,
					fmt.Errorf("signal tracked process %d: %w", identity.pid, err))
			}
			continue
		}
		signaled = true
	}
	return signaled, result
}

func signalProcessGroup(processGroupID int, signal syscall.Signal) error {
	if err := syscall.Kill(-processGroupID, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("signal process group: %w", err)
	}
	return nil
}
