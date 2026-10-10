package engineeringcontext

import (
	"encoding/json"
	"github.com/parametron-io/parametron-workflow/internal/semanticpolicy"
	"github.com/parametron-io/parametron-workflow/internal/storage"
)

func validateClassification(c semanticpolicy.IssueClassification) (semanticpolicy.IssueClassification, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	return semanticpolicy.DecodeIssueClassification(b)
}

// Kahn traversal is iterative: graph depth cannot overflow the call stack.
func cyclic(nodes map[storage.Resource]IssueNode, edges map[[2]storage.Resource]bool) bool {
	degree := map[storage.Resource]int{}
	next := map[storage.Resource][]storage.Resource{}
	for k := range nodes {
		degree[k] = 0
	}
	for pair := range edges {
		degree[pair[1]]++
		next[pair[0]] = append(next[pair[0]], pair[1])
	}
	queue := []storage.Resource{}
	for k, d := range degree {
		if d == 0 {
			queue = append(queue, k)
		}
	}
	visited := 0
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		visited++
		for _, target := range next[k] {
			degree[target]--
			if degree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	return visited != len(nodes)
}
