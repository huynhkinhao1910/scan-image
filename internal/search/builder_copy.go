package search

import "slices"

// copySubtreeLocked copies the old children of oldDir under newDir, then
// their descendants, breadth first, so parents get lower ids than their
// children and every directory's children stay one contiguous block. Flags
// of the copied entries are kept. It returns the number of directories
// visited and entries copied. The caller holds b.ix.mu.
func (b *Builder) copySubtreeLocked(oldDir, newDir uint32) (dirs, entries int) {
	type pair struct{ old, new uint32 }
	queue := []pair{{oldDir, newDir}}
	for head := 0; head < len(queue); head++ {
		p := queue[head]
		dirs++
		first, n := b.kids(p.old)
		if n == 0 {
			continue
		}
		newFirst := b.copyBlockLocked(first, n, p.new)
		entries += n
		for k := 0; k < n; k++ {
			if b.old.flags[first+k]&flagDir != 0 && b.first[first+k] != NoPrev {
				queue = append(queue, pair{uint32(first + k), newFirst + uint32(k)})
			}
		}
	}
	return dirs, entries
}

// copyBlockLocked appends old entries first..first+n-1 as children of
// parent, moving name bytes and numeric columns in bulk without re-folding.
func (b *Builder) copyBlockLocked(first, n int, parent uint32) uint32 {
	ix, o := b.ix, b.old
	newFirst := uint32(len(ix.parent))
	end := first + n

	lo, hi := o.nameOff[first], o.nameOff[end]
	delta := uint32(len(ix.names)) - lo
	ix.names = append(ix.names, o.names[lo:hi]...)
	for _, v := range o.nameOff[first+1 : end+1] {
		ix.nameOff = append(ix.nameOff, v+delta)
	}

	lo, hi = o.foldOff[first], o.foldOff[end]
	delta = uint32(len(ix.folded)) - lo
	ix.folded = append(ix.folded, o.folded[lo:hi]...)
	for _, v := range o.foldOff[first+1 : end+1] {
		ix.foldOff = append(ix.foldOff, v+delta)
	}

	ix.parent = append(ix.parent, slices.Repeat([]uint32{parent}, n)...)
	ix.size = append(ix.size, o.size[first:end]...)
	ix.mtime = append(ix.mtime, o.mtime[first:end]...)
	ix.flags = append(ix.flags, o.flags[first:end]...)
	for k := first; k < end; k++ {
		if o.flags[k]&flagDir != 0 {
			ix.nDirs++
		} else {
			ix.nFiles++
			ix.byteSum += o.size[k]
		}
	}
	return newFirst
}
