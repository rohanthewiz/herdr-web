package browserproto

import "github.com/rohanthewiz/herdr-web/internal/app"

// The §7 command vocabulary (names, param/result structs, direction mappings)
// now lives in internal/app so one command table can serve both this browser
// protocol and a future CLI/control-API. These are thin re-exports for wire use
// — existing browserproto consumers keep spelling browserproto.Cmd*/*Params.

// Command names (§7).
const (
	CmdPaneSplit          = app.CmdPaneSplit
	CmdPaneClose          = app.CmdPaneClose
	CmdPaneFocus          = app.CmdPaneFocus
	CmdPaneFocusDirection = app.CmdPaneFocusDirection
	CmdPaneCycle          = app.CmdPaneCycle
	CmdPaneLast           = app.CmdPaneLast
	CmdPaneSwap           = app.CmdPaneSwap
	CmdPaneZoom           = app.CmdPaneZoom
	CmdPaneRename         = app.CmdPaneRename
	CmdPaneResizeBorder   = app.CmdPaneResizeBorder
	CmdScroll             = app.CmdScroll
	CmdRead               = app.CmdRead
	CmdCapture            = app.CmdCapture
	CmdWaitForOutput      = app.CmdWaitForOutput
	CmdTabCreate          = app.CmdTabCreate
	CmdTabClose           = app.CmdTabClose
	CmdTabFocus           = app.CmdTabFocus
	CmdTabRename          = app.CmdTabRename
	CmdWorkspaceCreate    = app.CmdWorkspaceCreate
	CmdWorkspaceClose     = app.CmdWorkspaceClose
	CmdWorkspaceFocus     = app.CmdWorkspaceFocus
	CmdWorkspaceRename    = app.CmdWorkspaceRename
	CmdAgentFocus         = app.CmdAgentFocus
	CmdServerReloadConfig = app.CmdServerReloadConfig
	CmdServerStop         = app.CmdServerStop
	CmdSessionGet         = app.CmdSessionGet
	CmdWorkspaceList      = app.CmdWorkspaceList
	CmdTabList            = app.CmdTabList
	CmdPaneList           = app.CmdPaneList
	CmdPaneGet            = app.CmdPaneGet
)

// Split / cardinal direction wire values.
const (
	SplitH = app.SplitH
	SplitV = app.SplitV

	DirLeft  = app.DirLeft
	DirRight = app.DirRight
	DirUp    = app.DirUp
	DirDown  = app.DirDown
)

// Direction / border mappings (bound to the app implementations).
var (
	SplitDirection = app.SplitDirection
	NavDirection   = app.NavDirection
)

// Command param + result types.
type (
	SplitParams           = app.SplitParams
	PaneParams            = app.PaneParams
	OptPaneParams         = app.OptPaneParams
	DirParams             = app.DirParams
	CycleParams           = app.CycleParams
	RenamePaneParams      = app.RenamePaneParams
	ResizeBorderParams    = app.ResizeBorderParams
	ScrollParams          = app.ScrollParams
	ReadParams            = app.ReadParams
	ReadResult            = app.ReadResult
	CaptureParams         = app.CaptureParams
	CaptureResult         = app.CaptureResult
	WaitForOutputParams   = app.WaitForOutputParams
	WaitForOutputResult   = app.WaitForOutputResult
	TabParams             = app.TabParams
	OptTabParams          = app.OptTabParams
	RenameTabParams       = app.RenameTabParams
	WorkspaceParams       = app.WorkspaceParams
	RenameWorkspaceParams = app.RenameWorkspaceParams
)
