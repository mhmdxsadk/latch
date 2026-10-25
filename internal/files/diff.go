package files

import (
	"fmt"
)

// maxDiffCells bounds the work of the line matcher. A larger changed region
// is shown as all old lines removed and all new lines added.
const maxDiffCells = 4 << 20

// Diff returns a unified diff of a and b with ctx lines of context, without
// file headers: hunk headers ("@@ -1,3 +1,4 @@") and lines starting with
// " ", "-" or "+". It returns nil when a and b are equal.
func Diff(a, b []string, ctx int) []string {
	// Common prefix and suffix are cheap to find and usually most of a file.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	if pre == len(a) && pre == len(b) {
		return nil
	}

	type op struct {
		kind byte // ' ', '-', '+'
		ai   int  // line index in a (for ' ' and '-')
		bi   int  // line index in b (for ' ' and '+')
	}
	ops := make([]op, 0, len(a)+len(b))
	for i := range pre {
		ops = append(ops, op{' ', i, i})
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(am)*len(bm) > maxDiffCells || len(am) == 0 || len(bm) == 0 {
		for i := range am {
			ops = append(ops, op{'-', pre + i, 0})
		}
		for j := range bm {
			ops = append(ops, op{'+', 0, pre + j})
		}
	} else {
		// Longest common subsequence, filled from the end.
		w := len(bm) + 1
		lcs := make([]int32, (len(am)+1)*w)
		for i := len(am) - 1; i >= 0; i-- {
			for j := len(bm) - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				} else {
					lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < len(am) || j < len(bm) {
			switch {
			case i < len(am) && j < len(bm) && am[i] == bm[j]:
				ops = append(ops, op{' ', pre + i, pre + j})
				i++
				j++
			case i < len(am) && (j == len(bm) || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
				ops = append(ops, op{'-', pre + i, 0})
				i++
			default:
				ops = append(ops, op{'+', 0, pre + j})
				j++
			}
		}
	}
	for k := range suf {
		ops = append(ops, op{' ', len(a) - suf + k, len(b) - suf + k})
	}

	// apos[k] and bpos[k] count the lines of a and b before ops[k].
	apos := make([]int, len(ops)+1)
	bpos := make([]int, len(ops)+1)
	for k, o := range ops {
		apos[k+1], bpos[k+1] = apos[k], bpos[k]
		if o.kind != '+' {
			apos[k+1]++
		}
		if o.kind != '-' {
			bpos[k+1]++
		}
	}

	// Group changes into hunks with ctx lines of context around them.
	var out []string
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := max(k-ctx, 0)
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = run
		}
		out = append(out, fmt.Sprintf("@@ -%s +%s @@",
			hunkRange(apos[start], apos[end]-apos[start]), hunkRange(bpos[start], bpos[end]-bpos[start])))
		for _, o := range ops[start:end] {
			switch o.kind {
			case ' ':
				out = append(out, " "+a[o.ai])
			case '-':
				out = append(out, "-"+a[o.ai])
			case '+':
				out = append(out, "+"+b[o.bi])
			}
		}
		k = end
	}
	return out
}

// hunkRange formats "start,len" the way diff -u does: 1-based, the length
// left out when it is 1, and for an empty range the line before it.
func hunkRange(before, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return fmt.Sprintf("%d", before+1)
	}
	return fmt.Sprintf("%d,%d", before+1, n)
}
