//go:build linux

package hostsample

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func readCPU() (cpuTimes, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}, false
	}
	fields := strings.Fields(sc.Text()) // "cpu user nice system idle iowait irq softirq steal ..."
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	var total, idle float64
	for i, fld := range fields[1:] {
		v, err := strconv.ParseFloat(fld, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return cpuTimes{idle: idle, total: total}, true
}

func readRAM() (usedGiB, totalGiB float64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var total, avail float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb
		case "MemAvailable:":
			avail = kb
		}
	}
	if total <= 0 {
		return 0, 0, false
	}
	return (total - avail) / (1 << 20), total / (1 << 20), true
}
