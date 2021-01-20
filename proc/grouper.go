package proc

import (
	"time"

	common "github.com/ncabatoff/process-exporter"
	seq "github.com/ncabatoff/go-seq/seq"
)

type (
	// Grouper is the top-level interface to the process metrics.  All tracked
	// procs sharing the same group name are aggregated.
	Grouper struct {
		// groupAccum records the historical accumulation of a group so that
		// we can avoid ever decreasing the counts we return.
		groupAccum  map[string]Counts
		tracker     *Tracker
		threadAccum map[string]map[string]Threads
		debug       bool
	}

	// GroupByName maps group name to group metrics.
	GroupByName map[string]Group

	// Threads collects metrics for threads in a group sharing a thread name.
	Threads struct {
		Name       string
		NumThreads int
		Counts
	}

	GroupFiledescOpenCounts struct {
		// Sum is the count of open file descriptors of all types.
		Sum uint64
		// Files is the count of open file descriptors referrring to files.
		Files uint64
		// Sockets is the count of open file descriptors referrring to sockets.
		Sockets uint64
		// Pipes is the count of open file descriptors referrring to pipes.
		Pipes uint64
		// AnonInodes is the count of open file descriptors referrring to anonymous inodes.
		AnonInodes uint64
		// Unknown is the count of open file descriptors referrring to an unknown type.
		Unknown uint64
	}

	// Group describes the metrics of a single group.
	Group struct {
		Counts
		States
		Wchans map[string]int
		Procs  int
		Memory
		OldestStartTime time.Time
		OpenFDs         GroupFiledescOpenCounts
		WorstFDratio    float64
		NumThreads      uint64
		Threads         []Threads
	}
)

// Returns true if x < y.  Test designers should ensure they always have
// a unique name/numthreads combination for each group.
func lessThreads(x, y Threads) bool { return seq.Compare(x, y) < 0 }

// NewGrouper creates a grouper.
func NewGrouper(namer common.MatchNamer, trackChildren, trackThreads, alwaysRecheck, debug bool) *Grouper {
	g := Grouper{
		groupAccum:  make(map[string]Counts),
		threadAccum: make(map[string]map[string]Threads),
		tracker:     NewTracker(namer, trackChildren, trackThreads, alwaysRecheck, debug),
		debug:       debug,
	}
	return &g
}

func groupadd(grp Group, ts Update) Group {
	var zeroTime time.Time

	grp.Procs++
	grp.Memory.ResidentBytes += ts.Memory.ResidentBytes
	grp.Memory.VirtualBytes += ts.Memory.VirtualBytes
	grp.Memory.VmSwapBytes += ts.Memory.VmSwapBytes
	grp.Memory.ProportionalBytes += ts.Memory.ProportionalBytes
	grp.Memory.ProportionalSwapBytes += ts.Memory.ProportionalSwapBytes
	if ts.Filedesc.Open.Sum != -1 {
		grp.OpenFDs.Sum += uint64(ts.Filedesc.Open.Sum)
		openratio := float64(ts.Filedesc.Open.Sum) / float64(ts.Filedesc.Limit)
		if grp.WorstFDratio < openratio {
			grp.WorstFDratio = openratio
		}

		// These values will be > -1 if 'categorise FDs' is enabled
		if ts.Filedesc.Open.Files != -1 {
			grp.OpenFDs.Files += uint64(ts.Filedesc.Open.Files)
		}

		if ts.Filedesc.Open.Sockets != -1 {
			grp.OpenFDs.Sockets += uint64(ts.Filedesc.Open.Sockets)
		}

		if ts.Filedesc.Open.Pipes != -1 {
			grp.OpenFDs.Pipes += uint64(ts.Filedesc.Open.Pipes)
		}

		if ts.Filedesc.Open.AnonInodes != -1 {
			grp.OpenFDs.AnonInodes += uint64(ts.Filedesc.Open.AnonInodes)
		}

		if ts.Filedesc.Open.Unknown != -1 {
			grp.OpenFDs.AnonInodes += uint64(ts.Filedesc.Open.Unknown)
		}
	}

	grp.NumThreads += ts.NumThreads
	grp.Counts.Add(ts.Latest)
	grp.States.Add(ts.States)
	if grp.OldestStartTime == zeroTime || ts.Start.Before(grp.OldestStartTime) {
		grp.OldestStartTime = ts.Start
	}

	if grp.Wchans == nil {
		grp.Wchans = make(map[string]int)
	}
	for wchan, count := range ts.Wchans {
		grp.Wchans[wchan] += count
	}

	return grp
}

// Update asks the tracker to report on each tracked process by name.
// These are aggregated by groupname, augmented by accumulated counts
// from the past, and returned.  Note that while the Tracker reports
// only what counts have changed since last cycle, Grouper.Update
// returns counts that never decrease.  Even once the last process
// with name X disappears, name X will still appear in the results
// with the same counts as before; of course, all non-count metrics
// will be zero.
func (g *Grouper) Update(iter Iter) (CollectErrors, GroupByName, error) {
	cerrs, tracked, err := g.tracker.Update(iter)
	if err != nil {
		return cerrs, nil, err
	}
	return cerrs, g.groups(tracked), nil
}

// Translate the updates into a new GroupByName and update internal history.
func (g *Grouper) groups(tracked []Update) GroupByName {
	groups := make(GroupByName)
	threadsByGroup := make(map[string][]ThreadUpdate)

	for _, update := range tracked {
		groups[update.GroupName] = groupadd(groups[update.GroupName], update)
		if update.Threads != nil {
			threadsByGroup[update.GroupName] =
				append(threadsByGroup[update.GroupName], update.Threads...)
		}
	}

	// Add any accumulated counts to what was just observed,
	// and update the accumulators.
	for gname, group := range groups {
		if oldcounts, ok := g.groupAccum[gname]; ok {
			group.Counts.Add(Delta(oldcounts))
		}
		g.groupAccum[gname] = group.Counts
		group.Threads = g.threads(gname, threadsByGroup[gname])
		groups[gname] = group
	}

	// Now add any groups that were observed in the past but aren't running now.
	for gname, gcounts := range g.groupAccum {
		if _, ok := groups[gname]; !ok {
			groups[gname] = Group{Counts: gcounts}
		}
	}

	return groups
}

func (g *Grouper) threads(gname string, tracked []ThreadUpdate) []Threads {
	if len(tracked) == 0 {
		delete(g.threadAccum, gname)
		return nil
	}

	ret := make([]Threads, 0, len(tracked))
	threads := make(map[string]Threads)

	// First aggregate the thread metrics by thread name.
	for _, nc := range tracked {
		curthr := threads[nc.ThreadName]
		curthr.NumThreads++
		curthr.Counts.Add(nc.Latest)
		curthr.Name = nc.ThreadName
		threads[nc.ThreadName] = curthr
	}

	// Add any accumulated counts to what was just observed,
	// and update the accumulators.
	if history := g.threadAccum[gname]; history != nil {
		for tname := range threads {
			if oldcounts, ok := history[tname]; ok {
				counts := threads[tname]
				counts.Add(Delta(oldcounts.Counts))
				threads[tname] = counts
			}
		}
	}

	g.threadAccum[gname] = threads

	for _, thr := range threads {
		ret = append(ret, thr)
	}
	return ret
}
