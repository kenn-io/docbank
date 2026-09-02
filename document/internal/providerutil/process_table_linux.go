//go:build linux

package providerutil

import (
	"bytes"
	"os"
	"strconv"
)

func snapshotProcessTable() ([]processRecord, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	records := make([]processRecord, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		closingParenthesis := bytes.LastIndexByte(stat, ')')
		if closingParenthesis < 0 {
			continue
		}
		fields := bytes.Fields(stat[closingParenthesis+1:])
		if len(fields) < 20 {
			continue
		}
		parentID, parentErr := strconv.Atoi(string(fields[1]))
		startedAt, startErr := strconv.ParseUint(string(fields[19]), 10, 64)
		if parentErr != nil || startErr != nil {
			continue
		}
		records = append(records, processRecord{
			identity: processIdentity{pid: pid, startedAt: startedAt},
			parentID: parentID,
		})
	}
	return records, nil
}
