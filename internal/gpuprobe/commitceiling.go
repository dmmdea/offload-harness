package gpuprobe

// The commit CEILING: what the commit limit can reach, as opposed to where it stands.
//
// The commit limit a reading carries (HostMemory.CommitLimitGiB) is physical RAM plus the page files AT THEIR CURRENT SIZE. On a
// host whose page file may grow (Windows with a system-managed file, the default, or a custom file with a maximum above its
// size) that figure is not a wall: the memory manager extends the file when a commit request does not fit, and the request is
// granted. So the limit follows the charge. Measured on the reference 3-card Windows box (127.7 GiB RAM, one system-managed file):
//
//   - the limit read 146.7, 152.7, 154.7 and 160.8 GiB over two days while the commit charge moved between 55 and 141 GiB:
//     Windows kept it 14 to 28 GiB ahead of the charge and no further;
//   - a 19 GiB commit that touched nothing, asked for with 13.7 GiB of spare commit, was granted in 0.75 s: the limit went from
//     154.7 to 160.8 GiB, available memory did not move (67.1 GiB before and after) and page-file USE stayed at 0.2 GiB;
//   - the kernel reports the file's own bounds (SystemPageFileInformationEx): minimum 19.0, size 33.1, maximum 116.3 GiB, the
//     maximum being the volume's size / 8 that the vendor documents for a system-managed file.
//
// A rule that judges a commit against the CURRENT limit therefore asks whether the host holds, idle, the spare commit of the
// whole load, and a desktop never does: with 36 agent sessions open the box above sat at 125 to 141 GiB committed against a
// 153 to 161 GiB limit while 67 GiB of RAM was available, and an agent seat that commits 61 GiB (and touches 36) was refused on
// every contract for a day. What the OS can actually refuse is a commit past the limit WITH THE FILES AT THEIR MAXIMUM, and
// that is the line the rule holds a commit to: the ceiling.
//
//	ceiling = current limit + for each page file: min(maximum - size, free space of its volume - the reserve)
//
// A file at its maximum, a fixed-size file, a host with none and a platform whose reader cannot say all add nothing: there the
// ceiling IS the current limit and the rule is the one it was. Growing the page file is not paging: commit is a promise, the file
// backs it without being written until memory is short, and what pages is the touched set, which the other condition
// (available memory) guards exactly as before. The page file does get LARGER on disk when a grant leans on the ceiling, and it
// stays that size until the host restarts.

// PageFile is one page file as the kernel reports it.
type PageFile struct {
	// Path is the file ("C:\pagefile.sys") and Volume the root it sits on ("C:\"), the key its free space is read by.
	Path, Volume string
	// SizeBytes is its size now; MaxBytes the most the OS will grow it to (equal to SizeBytes for a fixed file).
	SizeBytes, MaxBytes uint64
}

// pageFileVolumeReserveShare is the share of a volume the ceiling never counts on a page file taking: growth is counted only
// while it leaves this much of the volume free. The OS would let the file take the volume to its end; a system volume with no
// free space is its own outage, so the rule does not plan on it. CHOSEN (a tenth, the level at which Windows itself starts
// warning about a volume), not measured; on the reference box it does not bind (338 GiB free of 930, 83 GiB of growth left).
const pageFileVolumeReserveShare = 0.10

// commitGrowthBytes is how far the page files can still grow: each file's maximum less its size, bounded by what its volume
// can give (free space less the reserve, shared between the files of one volume). space reads a volume's free and total bytes;
// a volume it cannot read gives nothing, so an unreadable host keeps the current limit as its ceiling.
func commitGrowthBytes(files []PageFile, space func(volume string) (free, total uint64, ok bool)) uint64 {
	left := map[string]uint64{}
	var grow uint64
	for _, f := range files {
		if f.MaxBytes <= f.SizeBytes {
			continue
		}
		usable, seen := left[f.Volume]
		if !seen {
			if free, total, ok := space(f.Volume); ok {
				if reserve := uint64(float64(total) * pageFileVolumeReserveShare); free > reserve {
					usable = free - reserve
				}
			}
		}
		room := min(f.MaxBytes-f.SizeBytes, usable)
		left[f.Volume] = usable - room
		grow += room
	}
	return grow
}

// commitCeilingGiB is the line a commit is judged against: the ceiling a reading carries when it is above the current limit,
// else the current limit (a reading that carries none, a fixed page file, a file at its maximum).
func (m HostMemory) commitCeilingGiB() float64 { return max(m.CommitLimitGiB, m.CommitCeilingGiB) }

// CommitCanGrow says the commit limit of this reading is not the wall: the page files can still take it higher.
func (m HostMemory) CommitCanGrow() bool { return m.CommitCeilingGiB > m.CommitLimitGiB }
