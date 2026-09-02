//go:build darwin

package providerutil

import "golang.org/x/sys/unix"

func snapshotProcessTable() ([]processRecord, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	records := make([]processRecord, 0, len(processes))
	for _, process := range processes {
		if process.Proc.P_pid <= 0 {
			continue
		}
		records = append(records, processRecord{
			identity: processIdentity{
				pid:       int(process.Proc.P_pid),
				startedAt: uint64(process.Proc.P_starttime.Nano()),
			},
			parentID: int(process.Eproc.Ppid),
		})
	}
	return records, nil
}
