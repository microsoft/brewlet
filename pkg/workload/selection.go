package workload

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/microsoft/brewlet/pkg/node"
)

const (
	RuntimeImageAnnotationPrefix = "runtime-image.brewlet.sh/"
	RuntimeGenerationPrefix      = "runtime-generation.brewlet.sh/"
)

var containerNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type Selection struct {
	ContainerName string
	RuntimeID     string
}

// ParseSelections validates every selection annotation, including reservations
// for containers that have not yet been added to the Pod.
func ParseSelections(annotations map[string]string) (map[string]string, error) {
	selections := make(map[string]string)
	for key, id := range annotations {
		if !strings.HasPrefix(key, RuntimeImageAnnotationPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, RuntimeImageAnnotationPrefix)
		if len(name) == 0 || len(name) > 63 || !containerNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid container name in runtime selection annotation %q", key)
		}
		if err := node.ValidateID(id); err != nil {
			return nil, fmt.Errorf("invalid runtime selection for container %q: %w", name, err)
		}
		selections[name] = id
	}
	return selections, nil
}

// RuntimeIDForContainer resolves the exact selection for a container. A missing
// selection is ordinary OCI; malformed selections are errors, never fallback.
func RuntimeIDForContainer(annotations map[string]string, containerName string) (string, bool, error) {
	selections, err := ParseSelections(annotations)
	if err != nil {
		return "", false, err
	}
	id, ok := selections[containerName]
	return id, ok, nil
}

// ActiveSelections returns only selections for declared container names.
func ActiveSelections(annotations map[string]string, declaredNames []string) ([]Selection, error) {
	selections, err := ParseSelections(annotations)
	if err != nil {
		return nil, err
	}
	active := make([]Selection, 0, len(selections))
	for _, name := range declaredNames {
		if id, ok := selections[name]; ok {
			active = append(active, Selection{ContainerName: name, RuntimeID: id})
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].ContainerName < active[j].ContainerName
	})
	return active, nil
}

func DistinctRuntimeIDs(selections []Selection) []string {
	set := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		set[selection.RuntimeID] = struct{}{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func RuntimeGenerationAnnotation(id string) string {
	return RuntimeGenerationPrefix + id
}
