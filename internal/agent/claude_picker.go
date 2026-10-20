package agent

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Claude Code 2.1.296 trims a description and treats an empty result as
// absent, then shows "Custom model (<route>)". A zero-width space remains
// after trim while rendering no source text in the picker.
const claudePickerBlankDescription = "\u200b"

// A suffix identifies a source, not a row or an account. All models of a
// provider share one; a routing group has its own since it may select more
// than one provider. Keep departed sources too, so their numbers are never
// reassigned when a provider is hidden, removed or later brought back.
func claudePickerSource(e provider.Entry) string {
	if e.Group != "" {
		return e.ID
	}
	return e.Provider.ID
}

func claudePickerLabels() (map[string]string, bool, error) {
	if !settings.Load().ClaudeMinimalModelPicker {
		return listLabels("claude"), false, nil
	}
	shown, _ := provider.CatalogFor("claude")
	var sources []string
	byName := map[string]map[string]bool{}
	labels := make(map[string]string, len(shown))
	for _, e := range shown {
		name := strings.TrimSpace(cmp.Or(e.PlainName(), e.Model))
		source := claudePickerSource(e)
		key := strings.ToLower(name)
		if byName[key] == nil {
			byName[key] = map[string]bool{}
		}
		byName[key][source] = true
		sources = append(sources, source)
		labels[e.ID] = name
	}
	numbers, err := claudePickerSourceNumbers(sources)
	if err != nil {
		return nil, true, err
	}
	for _, e := range shown {
		name := labels[e.ID]
		if len(byName[strings.ToLower(name)]) > 1 {
			labels[e.ID] = name + " v" + strconv.Itoa(numbers[claudePickerSource(e)])
		}
	}
	return labels, true, nil
}

var claudePickerSourcesMu sync.Mutex

func claudePickerSourcesPath() string {
	return filepath.Join(filepath.Dir(settings.Path()), "claude-picker-sources.json")
}

// Allocate before writing the picker, atomically and under both locks:
// the serving process and a CLI sync must agree on the same source ids.
// An unreadable registry stops the write instead of silently renumbering.
func claudePickerSourceNumbers(sources []string) (map[string]int, error) {
	claudePickerSourcesMu.Lock()
	defer claudePickerSourcesMu.Unlock()
	path := claudePickerSourcesPath()
	unlock, held := filelock.Lock(path+".lock", filelock.Wait)
	defer unlock()
	if !held {
		return nil, fmt.Errorf("could not lock Claude model source numbers at %s", path)
	}
	numbers := map[string]int{}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &numbers)
		if err == nil && numbers == nil {
			err = fmt.Errorf("expected an object of source numbers")
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("could not read Claude model source numbers: %w", err)
	}
	next := 0
	used := map[int]string{}
	for source, n := range numbers {
		if source == "" || n <= 0 || used[n] != "" {
			return nil, fmt.Errorf("invalid or repeated Claude model source number for %q", source)
		}
		used[n] = source
		next = max(next, n)
	}
	sources = slices.Clone(sources)
	slices.Sort(sources)
	changed := false
	for _, source := range slices.Compact(sources) {
		if source == "" {
			return nil, fmt.Errorf("Claude model has no source")
		}
		if numbers[source] == 0 {
			next++
			if next <= 0 {
				return nil, fmt.Errorf("Claude model source numbers exhausted")
			}
			numbers[source] = next
			changed = true
		}
	}
	if changed {
		b, err = json.MarshalIndent(numbers, "", "  ")
		if err == nil {
			err = edit.WriteAtomic(path, append(b, '\n'))
		}
		if err != nil {
			return nil, fmt.Errorf("could not save Claude model source numbers: %w", err)
		}
	}
	return numbers, nil
}
