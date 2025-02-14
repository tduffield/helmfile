package state

import (
	"fmt"
	"strings"
	"sync"

	"github.com/roboll/helmfile/pkg/helmexec"
	"github.com/variantdev/dag/pkg/dag"
)

type result struct {
	release ReleaseSpec
	err     error
}

func (st *HelmState) scatterGather(concurrency int, items int, produceInputs func(), receiveInputsAndProduceIntermediates func(int), aggregateIntermediates func()) {

	if concurrency < 1 || concurrency > items {
		concurrency = items
	}

	for _, r := range st.Releases {
		if r.Tillerless != nil {
			if *r.Tillerless {
				concurrency = 1
			}
		} else if st.HelmDefaults.Tillerless {
			concurrency = 1
		}
	}

	// WaitGroup is required to wait until goroutine per job in job queue cleanly stops.
	var waitGroup sync.WaitGroup
	waitGroup.Add(concurrency)

	go produceInputs()

	for w := 1; w <= concurrency; w++ {
		go func(id int) {
			st.logger.Debugf("worker %d/%d started", id, concurrency)
			receiveInputsAndProduceIntermediates(id)
			st.logger.Debugf("worker %d/%d finished", id, concurrency)
			waitGroup.Done()
		}(w)
	}

	aggregateIntermediates()

	// Wait until all the goroutines to gracefully finish
	waitGroup.Wait()
}

func (st *HelmState) scatterGatherReleases(helm helmexec.Interface, concurrency int,
	do func(ReleaseSpec, int) error) []error {

	return st.iterateOnReleases(helm, concurrency, st.Releases, do)
}

func (st *HelmState) iterateOnReleases(helm helmexec.Interface, concurrency int, inputs []ReleaseSpec,
	do func(ReleaseSpec, int) error) []error {
	var errs []error

	inputsSize := len(inputs)

	releases := make(chan ReleaseSpec)
	results := make(chan result)

	st.scatterGather(
		concurrency,
		inputsSize,
		func() {
			for _, release := range inputs {
				releases <- release
			}
			close(releases)
		},
		func(id int) {
			for release := range releases {
				err := do(release, id)
				st.logger.Debugf("sending result for release: %s\n", release.Name)
				results <- result{release: release, err: err}
				st.logger.Debugf("sent result for release: %s\n", release.Name)
			}
		},
		func() {
			for i := range inputs {
				st.logger.Debugf("receiving result %d", i)
				r := <-results
				if r.err != nil {
					errs = append(errs, fmt.Errorf("release \"%s\" failed: %v", r.release.Name, r.err))
				} else {
					st.logger.Debugf("received result for release \"%s\"", r.release.Name)
				}
				st.logger.Debugf("received result for %d", i)
			}
		},
	)

	if len(errs) != 0 {
		return errs
	}

	return nil
}

func (st *HelmState) dagAwareReverseIterateOnReleases(helm helmexec.Interface, concurrency int,
	do func(ReleaseSpec, int) error) []error {

	idToRelease := map[string]ReleaseSpec{}

	preps := st.Releases

	d := dag.New()
	for _, r := range preps {

		id := releaseToID(&r)

		idToRelease[id] = r

		d.Add(id, dag.Dependencies(r.Needs))
	}

	plan, err := d.Plan()
	if err != nil {
		return []error{err}
	}

	groupsTotal := len(plan)

	st.logger.Debugf("processing %d groups of releases in this order: %s", groupsTotal, plan)

	for groupIndex := len(plan) - 1; groupIndex >= 0; groupIndex-- {
		dagNodesInGroup := plan[groupIndex]

		var idsInGroup []string
		var releasesInGroup []ReleaseSpec

		for _, node := range dagNodesInGroup {
			releasesInGroup = append(releasesInGroup, idToRelease[node.Id])
			idsInGroup = append(idsInGroup, node.Id)
		}

		st.logger.Debugf("processing releases in group %d/%d: %s", groupIndex+1, groupsTotal, strings.Join(idsInGroup, ", "))

		errs := st.iterateOnReleases(helm, concurrency, releasesInGroup, do)

		if len(errs) > 0 {
			return errs
		}
	}

	return nil
}
