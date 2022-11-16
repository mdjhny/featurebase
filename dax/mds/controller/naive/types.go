package naive

import (
	"sort"

	"github.com/molecula/featurebase/v3/dax"
)

// jobSetDiffs is used internally to capture the diffs as they're happening. We
// call output() to generate the final result.
type jobSetDiffs struct {
	added   dax.Set[dax.Job]
	removed dax.Set[dax.Job]
}

func newJobSetDiffs() jobSetDiffs {
	return jobSetDiffs{
		added:   dax.NewSet[dax.Job](),
		removed: dax.NewSet[dax.Job](),
	}
}

type internalDiffs map[dax.Worker]jobSetDiffs

func newInternalDiffs() internalDiffs {
	return make(internalDiffs)
}

func (d internalDiffs) added(worker dax.Worker, job dax.Job) {
	if _, ok := d[worker]; !ok {
		d[worker] = newJobSetDiffs()
	}

	// Before adding the job, make sure we haven't indicated that it has been
	// removed prior to this. If it has, we need to invalidate that "remove"
	// instruction.
	d[worker].removed.Remove(job)

	d[worker].added.Add(job)
}

func (d internalDiffs) removed(worker dax.Worker, job dax.Job) {
	if _, ok := d[worker]; !ok {
		d[worker] = newJobSetDiffs()
	}

	// Before removing the job, make sure we haven't indicated that it has been
	// added prior to this. If it has, we need to invalidate that "add"
	// instruction.
	d[worker].added.Remove(job)

	d[worker].removed.Add(job)
}

func (d internalDiffs) merge(d2 internalDiffs) {
	for k, v := range d2 {
		if _, ok := d[k]; !ok {
			d[k] = newJobSetDiffs()
		}
		d[k].added.Merge(v.added)
		d[k].removed.Merge(v.removed)
	}
}

// output converts internalDiff to []controller.WorkerDiff for external
// consumption.
func (d internalDiffs) output() []dax.WorkerDiff {
	out := make([]dax.WorkerDiff, len(d))

	i := 0
	for k, v := range d {
		out[i].WorkerID = k
		out[i].AddedJobs = v.added.Sorted()
		out[i].RemovedJobs = v.removed.Sorted()
		i++
	}

	sort.Sort(dax.WorkerDiffs(out))

	return out
}