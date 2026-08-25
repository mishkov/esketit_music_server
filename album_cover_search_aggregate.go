package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
)

type albumCoverSearchSource struct {
	name     string
	provider albumCoverSearchProvider
}

type multiAlbumCoverSearchProvider struct {
	sources []albumCoverSearchSource
}

type albumCoverSearchOutcome struct {
	index int
	items []albumCoverSuggestion
	err   error
}

type albumCoverSearchProviderFailure struct {
	source string
	err    error
}

type albumCoverSearchProvidersError struct {
	failures []albumCoverSearchProviderFailure
}

func (e *albumCoverSearchProvidersError) Error() string {
	if e == nil || len(e.failures) == 0 {
		return "all album cover search providers failed"
	}

	parts := make([]string, 0, len(e.failures))
	for _, failure := range e.failures {
		parts = append(parts, fmt.Sprintf("%s: %v", failure.source, failure.err))
	}
	return "all album cover search providers failed: " + strings.Join(parts, "; ")
}

func (e *albumCoverSearchProvidersError) Unwrap() []error {
	if e == nil {
		return nil
	}

	errs := make([]error, 0, len(e.failures))
	for _, failure := range e.failures {
		if failure.err != nil {
			errs = append(errs, fmt.Errorf("%s album cover search: %w", failure.source, failure.err))
		}
	}
	return errs
}

func (e *albumCoverSearchProvidersError) allFailuresMatch(target error) bool {
	if e == nil || len(e.failures) == 0 {
		return false
	}
	for _, failure := range e.failures {
		if !errors.Is(failure.err, target) {
			return false
		}
	}
	return true
}

func newMultiAlbumCoverSearchProvider(sources []albumCoverSearchSource) albumCoverSearchProvider {
	usable := make([]albumCoverSearchSource, 0, len(sources))
	for _, source := range sources {
		source.name = strings.ToLower(strings.TrimSpace(source.name))
		if source.name == "" || source.provider == nil {
			continue
		}
		usable = append(usable, source)
	}
	if len(usable) == 0 {
		return nil
	}
	return &multiAlbumCoverSearchProvider{sources: usable}
}

func (p *multiAlbumCoverSearchProvider) Search(ctx context.Context, query string, limit int) ([]albumCoverSuggestion, error) {
	if p == nil || len(p.sources) == 0 {
		return nil, errAlbumCoverSuggestionsUnavailable
	}
	if limit <= 0 {
		return []albumCoverSuggestion{}, nil
	}

	outcomes := make(chan albumCoverSearchOutcome, len(p.sources))
	for index, source := range p.sources {
		go func(index int, source albumCoverSearchSource) {
			items, err := source.provider.Search(ctx, query, limit)
			outcomes <- albumCoverSearchOutcome{index: index, items: items, err: err}
		}(index, source)
	}

	ordered := make([]albumCoverSearchOutcome, len(p.sources))
	for range p.sources {
		outcome := <-outcomes
		ordered[outcome.index] = outcome
	}

	groups := make([][]albumCoverSuggestion, 0, len(p.sources))
	failures := make([]albumCoverSearchProviderFailure, 0, len(p.sources))
	for index, outcome := range ordered {
		source := p.sources[index]
		if outcome.err != nil {
			failures = append(failures, albumCoverSearchProviderFailure{source: source.name, err: outcome.err})
			log.Printf("album cover suggestion provider failed source=%s error=%s", source.name, safeOperationalError(outcome.err))
			continue
		}

		items := append([]albumCoverSuggestion(nil), outcome.items...)
		for itemIndex := range items {
			items[itemIndex].Source = source.name
		}
		groups = append(groups, items)
	}

	if len(groups) == 0 {
		return nil, &albumCoverSearchProvidersError{failures: failures}
	}
	return allocateAlbumCoverSuggestions(groups, limit), nil
}

func allocateAlbumCoverSuggestions(groups [][]albumCoverSuggestion, limit int) []albumCoverSuggestion {
	if limit <= 0 {
		return []albumCoverSuggestion{}
	}

	counts := make([]int, len(groups))
	active := make([]int, 0, len(groups))
	for index, group := range groups {
		if len(group) > 0 {
			active = append(active, index)
		}
	}

	remaining := limit
	for remaining > 0 && len(active) > 0 {
		share := remaining / len(active)
		extra := remaining % len(active)
		nextActive := make([]int, 0, len(active))
		allocated := 0

		for position, index := range active {
			requested := share
			if position < extra {
				requested++
			}
			available := len(groups[index]) - counts[index]
			if requested > available {
				requested = available
			}
			if requested > 0 {
				counts[index] += requested
				remaining -= requested
				allocated += requested
			}
			if counts[index] < len(groups[index]) {
				nextActive = append(nextActive, index)
			}
		}

		if allocated == 0 {
			break
		}
		active = nextActive
	}

	total := 0
	for _, count := range counts {
		total += count
	}
	items := make([]albumCoverSuggestion, 0, total)
	for index, count := range counts {
		items = append(items, groups[index][:count]...)
	}
	return items
}

func albumCoverSearchFailuresOnlyMatch(err, target error) bool {
	var providerErrors *albumCoverSearchProvidersError
	if errors.As(err, &providerErrors) {
		return providerErrors.allFailuresMatch(target)
	}
	return errors.Is(err, target)
}
