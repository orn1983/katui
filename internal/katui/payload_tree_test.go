package katui

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestFullTreeGroupsEveryChild(t *testing.T) {
	const last = 60_202
	obj := make(map[string]any, last+1)
	array := make([]any, last+1)
	for i := range array {
		obj[fmt.Sprintf("key%05d", i)] = i
		array[i] = i
	}
	for _, tc := range []struct {
		name  string
		value any
		leaf  string
	}{{"array", array, "[60202]"}, {"object", obj, "key60202"}} {
		t.Run(tc.name, func(t *testing.T) {
			n, limited := buildTree(t.Context(), "root", tc.value, true)
			if limited {
				t.Fatal("full tree was limited")
			}
			for {
				children := n.childNodes()
				if len(children) == 0 {
					break
				}
				if len(children) > 100 {
					t.Fatalf("%d children at %s", len(children), n.label)
				}
				child := children[len(children)-1]
				if child.parent != n || n.childNodes()[len(children)-1] != child {
					t.Fatal("lost node identity or parent")
				}
				n = child
				if subset, ok := n.value.(objectRange); ok {
					raw, err := marshalJSONNode(t.Context(), subset, math.MaxInt)
					var rendered map[string]int
					if err != nil || json.Unmarshal(raw, &rendered) != nil || len(rendered) != len(subset.keys) || rendered["key60202"] != last {
						t.Fatal("object range renders the wrong subset")
					}
				}
			}
			if n.search != tc.leaf+" 60202" {
				t.Fatalf("wrong final leaf: %s", n.search)
			}
		})
	}
}

func TestSplitTreePreviewBudget(t *testing.T) {
	values := make([]any, 2000)
	root, limited := buildTree(t.Context(), "root", values, false)
	if !limited || len(root.childNodes()) != 1999 {
		t.Fatal("split preview did not stop at its node budget")
	}
	root, limited = buildTree(t.Context(), "root", values[:1999], false)
	if limited || len(root.childNodes()) != 1999 {
		t.Fatal("split preview reported truncation without omitting a node")
	}
}

func TestDeepFullTreeVisibleAfterResize(t *testing.T) {
	var data any = "needle"
	for range 256 {
		data = []any{data}
	}
	root, limited := buildTree(t.Context(), "root", []any{data, "shallow sibling"}, true)
	if limited {
		t.Fatal("full tree was limited")
	}
	pane := &payloadPane{}
	tree := pane.newTreeView()
	tree.SetChangedFunc(nil) // Isolate rendering from the asynchronous node text view.
	tree.SetRoot(root.treeWidget())
	n := root
	for {
		pane.materialize(n)
		n.widget.Expand()
		children := n.childNodes()
		if len(children) == 0 {
			break
		}
		n = children[0]
	}
	if n.value != "needle" {
		t.Fatalf("wrong leaf at depth %d: %v", n.widgetDepth, n.value)
	}
	tree.SetCurrentNode(n.widget)
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	for _, width := range []int{80, 24} {
		screen.SetSize(width, 12)
		tree.SetRect(0, 0, width, 12)
		tree.Draw(screen)
		screen.Show()
		cells, _, _ := screen.GetContents()
		var text strings.Builder
		for _, c := range cells {
			text.WriteString(string(c.Runes))
		}
		if !strings.Contains(text.String(), "needle") {
			t.Fatalf("%d-column viewport hid selected leaf: %q", width, text.String())
		}
	}
	// The row immediately after the deep leaf is a child of the root.
	tree.InputHandler()(tcell.NewEventKey(tcell.KeyDown, 0, 0), nil)
	tree.Draw(screen)
	if tree.GetCurrentNode() != root.childNodes()[1].widget {
		t.Fatal("could not move from the deeply nested leaf to its shallow sibling")
	}
	tree.SetCurrentNode(root.widget)
	tree.Draw(screen)
	if tree.GetCurrentNode() != root.widget {
		t.Fatal("could not navigate back to root")
	}
}
