//go:build windows

package gpuprobe

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemPageFileInformationEx is the NtQuerySystemInformation class that lists the page files with the bounds the memory
// manager grows each one between. The plain class (18) carries the size and the use only; the registry and WMI say "0 0" for a
// system-managed file, which is the case that matters.
const systemPageFileInformationEx = 144

// pageFileInfoEx mirrors SYSTEM_PAGEFILE_INFORMATION_EX: one entry per page file, chained by NextEntryOffset, sizes in pages.
type pageFileInfoEx struct {
	NextEntryOffset uint32
	TotalSize       uint32
	TotalInUse      uint32
	PeakUsage       uint32
	PageFileName    windows.NTUnicodeString
	MinimumSize     uint32
	MaximumSize     uint32
}

// queryPageFiles asks the kernel for the page-file list into buf and says how many bytes it wrote. A variable so a test can
// stand a list in; production never replaces it.
var queryPageFiles = func(buf []byte) (n uint32, err error) {
	err = windows.NtQuerySystemInformation(systemPageFileInformationEx, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
	return n, err
}

// readPageFiles lists the host's page files with their sizes and maxima. ok=false means the kernel did not answer (the ceiling
// is then the current limit); an empty list with ok=true is a host with no page file.
func readPageFiles() ([]PageFile, bool) {
	buf := make([]byte, 4096)
	var n uint32
	for try := 0; ; try++ {
		var err error
		if n, err = queryPageFiles(buf); err == nil {
			break
		}
		if err != windows.STATUS_INFO_LENGTH_MISMATCH || try == 3 {
			return nil, false
		}
		buf = make([]byte, max(int(n), 2*len(buf)))
	}
	page := uint64(windows.Getpagesize())
	entry := uint32(unsafe.Sizeof(pageFileInfoEx{}))
	var files []PageFile
	for off := uint32(0); off+entry <= n && off+entry <= uint32(len(buf)); {
		e := (*pageFileInfoEx)(unsafe.Pointer(&buf[off]))
		if name := e.PageFileName.String(); name != "" {
			path := strings.TrimPrefix(name, `\??\`)
			files = append(files, PageFile{Path: path, Volume: filepath.VolumeName(path) + `\`,
				SizeBytes: uint64(e.TotalSize) * page, MaxBytes: uint64(e.MaximumSize) * page})
		}
		if e.NextEntryOffset == 0 {
			break
		}
		off += e.NextEntryOffset
	}
	return files, true
}

// volumeSpace reads a volume's free and total bytes (the whole volume's, not a quota's).
func volumeSpace(volume string) (free, total uint64, ok bool) {
	root, err := windows.UTF16PtrFromString(volume)
	if err != nil {
		return 0, 0, false
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(root, &avail, &total, &free); err != nil {
		return 0, 0, false
	}
	return free, total, true
}

// commitGrowthGiB is how far the page files can still take the commit limit, in GiB; 0 when the kernel does not say.
func commitGrowthGiB() float64 {
	files, ok := readPageFiles()
	if !ok {
		return 0
	}
	return float64(commitGrowthBytes(files, volumeSpace)) / (1 << 30)
}
