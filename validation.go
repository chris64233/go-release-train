package releasetrain

import (
	"fmt"
	"sort"
)

// versionLookup 按 (组件, 版本) 查已登记组件版本的约束声明。
type versionLookup interface {
	constraintsOf(component string, v Version) ([]Constraint, bool)
}

// validateSnapshot 对一份候选快照做冻结前的完整校验：
//  1. 快照内每个组件版本都必须已登记；
//  2. 该版本声明的每一条约束，其目标组件必须在快照内，且快照选用的版本满足约束；
//  3. 由快照内组件间的约束边构成的依赖图必须无环。
//
// 所有问题一次性收集后统一返回——任何一个组件不兼容都会让冻结整体失败，
// 不会留下部分冻结结果。
func validateSnapshot(snapshot map[string]Version, reg versionLookup) []string {
	var problems []string

	names := snapshotComponents(snapshot)

	// 1 & 2：登记存在性 + 约束满足。
	edges := make(map[string][]string)
	for _, name := range names {
		v := snapshot[name]
		cs, ok := reg.constraintsOf(name, v)
		if !ok {
			problems = append(problems, fmt.Sprintf("component %s version %s is not registered", name, v))
			continue
		}
		for _, c := range cs {
			target, present := snapshot[c.Component]
			if !present {
				problems = append(problems, fmt.Sprintf(
					"component %s %s requires %s %s, but %s is not in the candidate snapshot",
					name, v, c.Component, string(c.Op)+" "+c.Version.String(), c.Component))
				continue
			}
			if !c.SatisfiedBy(target) {
				problems = append(problems, fmt.Sprintf(
					"component %s %s requires %s %s, but snapshot has %s %s",
					name, v, c.Component, string(c.Op)+" "+c.Version.String(), c.Component, target))
			}
			// 约束边只在快照内部组件之间建立；指向快照外的边不构成环（且上面已报错）。
			if present {
				edges[name] = append(edges[name], c.Component)
			}
		}
	}

	// 3：Kahn 拓扑排序检测环。有登记缺失时仍可检测结构环，两种问题都要报。
	if cycle := findCycle(names, edges); cycle != nil {
		problems = append(problems, fmt.Sprintf("dependency cycle detected: %v", cycle))
	}

	return problems
}

// findCycle 用 Kahn 算法找环；无环返回 nil，有环返回环上（无法被入度消解）的节点。
func findCycle(nodes []string, edges map[string][]string) []string {
	indeg := make(map[string]int, len(nodes))
	for _, n := range nodes {
		indeg[n] = 0
	}
	// 去重边，避免重复约束抬高入度。
	adj := make(map[string]map[string]struct{}, len(nodes))
	for from, tos := range edges {
		for _, to := range tos {
			if _, ok := indeg[to]; !ok {
				continue // 指向快照外节点，忽略
			}
			if adj[from] == nil {
				adj[from] = map[string]struct{}{}
			}
			if _, seen := adj[from][to]; !seen {
				adj[from][to] = struct{}{}
				indeg[to]++
			}
		}
	}

	queue := make([]string, 0)
	for _, n := range nodes {
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}

	visited := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		visited++
		for to := range adj[cur] {
			indeg[to]--
			if indeg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}

	if visited == len(nodes) {
		return nil
	}
	cyclic := make([]string, 0)
	for _, n := range nodes {
		if indeg[n] > 0 {
			cyclic = append(cyclic, n)
		}
	}
	sort.Strings(cyclic)
	return cyclic
}
