package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Tests for the 2026-09-19 wrong-checkout edit incident: a session whose cwd
// sits inside a linked worktree of a tracked repository must never have its
// mutations served from the base corpus. The base corpus path resolvers
// anchor prefixed and repo-relative paths against the MAIN checkout's root,
// and the worktreeRootedPath existence heuristic then leaves the file there
// when it exists in both checkouts — silently editing the wrong copy.
//
// The binding happy path (cwd inside a ready+automatic checkout, route
// materializable, bytes land in the working copy) is already pinned
// end-to-end by TestWorktreeMutationCoordinatorEndToEnd's "cwd" subtest.
// These tests pin the NEW defence: when the route cannot serve, a mutation
// refuses loudly (view_building) instead of degrading to base, and a read
// degrades only with a labelled rider.

// retireRoute flips the worktree checkout's route to Retired with both
// generation slots zeroed: the checkout row itself stays ready+automatic,
// but no generation stack can serve it anymore.
func retireRoute(t *testing.T, stack *viewStack) {
	t.Helper()
	require.NoError(t, stack.srv.materializer.Catalog.UpsertCheckoutRoute(
		context.Background(), store_sqlite.CheckoutRoute{
			CheckoutID: viewTestWorktree,
			GraphID:    stack.graphID,
			State:      store_sqlite.RouteRetired,
		}))
}

func editFileViaMiddleware(stack *viewStack, cwd string, args map[string]any) (*mcplib.CallToolResult, error) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = args
	ctx := WithAuthorizedToolCall(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd),
		"edit_file")
	return stack.srv.wrapToolHandler(stack.srv.handleEditFile)(ctx, req)
}

func TestCWDBindingRouteNotReadyMutationsRefuseLoudly(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	// Divergent exists-in-both: the corpus root owns "func Old() {}" while
	// the worktree carries a different body. Before the defence this state
	// silently wrote the main copy.
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\n// worktree copy\n"), 0o644))

	result, err := editFileViaMiddleware(stack, stack.worktreeRoot, map[string]any{
		"path":       "repo/edit.go",
		"old_string": "func Old() {}",
		"new_string": "func Never() {}",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.IsError,
		"a mutation on an unrouted checkout must refuse, not silently use base")
	text := viewResultText(t, result)
	require.Contains(t, text, graphview.CodeViewBuilding,
		"refusal must carry view_building, got: %s", text)

	// And crucially: no bytes moved anywhere.
	mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(mainAfter), "func Never() {}",
		"the refused edit still wrote the MAIN copy")
	worktreeAfter, readErr := os.ReadFile(filepath.Join(stack.worktreeRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(worktreeAfter), "func Never() {}",
		"the refused edit still wrote the worktree copy")
}

// TestCWDBindingRouteNotReadyReadFileIsLabeled pins the incident's read
// twin: a read_file with a repo-prefixed path from a worktree-anchored
// session whose route cannot serve must degrade to base WITH the rider
// (visible degradation), and the rider must say which checkout the session
// actually wanted so the client can reconstruct the misroute.
func TestCWDBindingRouteNotReadyReadFileIsLabeled(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": "repo/edit.go"}
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	res, err := stack.srv.wrapToolHandler(stack.srv.handleReadFile)(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "a prefixed read may degrade to base: %s", viewResultText(t, res))

	// The bytes themselves come from the base corpus (main checkout's index
	// state) — that is the declared degradation. What must NOT happen is an
	// exact-looking answer: the rider says so.
	rider := resultFreshness(t, res)
	require.NotNil(t, rider, "degraded read_file must say so on the rider")
	require.Equal(t, "worktree:"+viewTestWorktree, rider["requested_view"],
		"the rider must name the checkout the session actually bound, not just auto")
	require.Equal(t, false, rider["exact"])
	require.Equal(t, graphview.CodeViewBuilding, rider["fallback_reason"])
	require.Equal(t, viewTestWorktree, rider["checkout_id"],
		"rider must name the checkout the cwd wanted, so the client sees the misroute")
	require.NotEqual(t, "", rider["graph_id"],
		"single-family fallback names the primary base graph it answered from")
}

func TestCWDBindingRouteNotReadyReadsFallBackWithRider(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
		nil, captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, "a read on an unrouted checkout may degrade to base: %s", viewResultText(t, res))
	rider := resultFreshness(t, res)
	require.NotNil(t, rider, "degraded read must say so on the rider")
	require.Equal(t, string(graphview.SelectorBase), rider["actual_view"])
	require.Equal(t, false, rider["exact"], "fallback must not claim exact")
	require.Equal(t, graphview.CodeViewBuilding, rider["fallback_reason"])
}
