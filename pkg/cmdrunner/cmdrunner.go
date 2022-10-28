package cmdrunner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alessio/shellescape"
	"github.com/google/uuid"
	"github.com/scripthaus-dev/mshell/pkg/base"
	"github.com/scripthaus-dev/mshell/pkg/packet"
	"github.com/scripthaus-dev/mshell/pkg/shexec"
	"github.com/scripthaus-dev/sh2-server/pkg/remote"
	"github.com/scripthaus-dev/sh2-server/pkg/scbase"
	"github.com/scripthaus-dev/sh2-server/pkg/scpacket"
	"github.com/scripthaus-dev/sh2-server/pkg/sstore"
)

const (
	HistoryTypeWindow  = "window"
	HistoryTypeSession = "session"
	HistoryTypeGlobal  = "global"
)

const DefaultUserId = "sawka"
const MaxNameLen = 50
const MaxRemoteAliasLen = 50
const PasswordUnchangedSentinel = "--unchanged--"

var ColorNames = []string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white", "orange"}
var RemoteColorNames = []string{"red", "green", "yellow", "blue", "magenta", "cyan", "white", "orange"}
var RemoteSetArgs = []string{"alias", "connectmode", "key", "password", "autoinstall", "color"}

var WindowCmds = []string{"run", "comment", "cd", "cr", "clear", "sw", "alias", "unalias", "function", "reset"}
var NoHistCmds = []string{"compgen", "line", "history"}
var GlobalCmds = []string{"session", "screen", "remote"}

var hostNameRe = regexp.MustCompile("^[a-z][a-z0-9.-]*$")
var userHostRe = regexp.MustCompile("^(sudo@)?([a-z][a-z0-9-]*)@([a-z][a-z0-9.-]*)(?::([0-9]+))?$")
var remoteAliasRe = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9_-]*$")
var genericNameRe = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9_ .()<>,/\"'\\[\\]{}=+$@!*-]*$")
var positionRe = regexp.MustCompile("^((S?\\+|E?-)?[0-9]+|(\\+|-|S|E))$")
var wsRe = regexp.MustCompile("\\s+")

type contextType string

var historyContextKey = contextType("history")

type historyContextType struct {
	LineId    string
	CmdId     string
	RemotePtr *sstore.RemotePtrType
}

type MetaCmdFnType = func(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error)
type MetaCmdEntryType struct {
	IsAlias bool
	Fn      MetaCmdFnType
}

var MetaCmdFnMap = make(map[string]MetaCmdEntryType)

func init() {
	registerCmdFn("run", RunCommand)
	registerCmdFn("eval", EvalCommand)
	registerCmdFn("comment", CommentCommand)
	// registerCmdFn("cd", CdCommand)
	registerCmdFn("cr", CrCommand)
	registerCmdFn("compgen", CompGenCommand)
	registerCmdFn("clear", ClearCommand)
	registerCmdFn("reset", ResetCommand)

	registerCmdFn("session", SessionCommand)
	registerCmdFn("session:open", SessionOpenCommand)
	registerCmdAlias("session:new", SessionOpenCommand)
	registerCmdFn("session:set", SessionSetCommand)
	registerCmdFn("session:delete", SessionDeleteCommand)

	registerCmdFn("screen", ScreenCommand)
	registerCmdFn("screen:close", ScreenCloseCommand)
	registerCmdFn("screen:open", ScreenOpenCommand)
	registerCmdAlias("screen:new", ScreenOpenCommand)
	registerCmdFn("screen:set", ScreenSetCommand)

	registerCmdAlias("remote", RemoteCommand)
	registerCmdFn("remote:show", RemoteShowCommand)
	registerCmdFn("remote:showall", RemoteShowAllCommand)
	registerCmdFn("remote:new", RemoteNewCommand)
	registerCmdFn("remote:archive", RemoteArchiveCommand)
	registerCmdFn("remote:set", RemoteSetCommand)
	registerCmdAlias("remote:edit", RemoteSetCommand)
	registerCmdFn("remote:disconnect", RemoteDisconnectCommand)
	registerCmdFn("remote:connect", RemoteConnectCommand)
	registerCmdFn("remote:install", RemoteInstallCommand)
	registerCmdFn("remote:installcancel", RemoteInstallCancelCommand)

	registerCmdFn("sw:set", SwSetCommand)
	registerCmdFn("sw:resize", SwResizeCommand)

	// sw:resize
	registerCmdFn("window:resize", SwResizeCommand)

	registerCmdFn("line", LineCommand)
	registerCmdFn("line:show", LineShowCommand)

	registerCmdFn("history", HistoryCommand)
}

func getValidCommands() []string {
	var rtn []string
	for key, val := range MetaCmdFnMap {
		if val.IsAlias {
			continue
		}
		rtn = append(rtn, "/"+key)
	}
	return rtn
}

func registerCmdFn(cmdName string, fn MetaCmdFnType) {
	MetaCmdFnMap[cmdName] = MetaCmdEntryType{Fn: fn}
}

func registerCmdAlias(cmdName string, fn MetaCmdFnType) {
	MetaCmdFnMap[cmdName] = MetaCmdEntryType{IsAlias: true, Fn: fn}
}

func HandleCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	metaCmd := SubMetaCmd(pk.MetaCmd)
	var cmdName string
	if pk.MetaSubCmd == "" {
		cmdName = metaCmd
	} else {
		cmdName = fmt.Sprintf("%s:%s", pk.MetaCmd, pk.MetaSubCmd)
	}
	entry := MetaCmdFnMap[cmdName]
	if entry.Fn == nil {
		if MetaCmdFnMap[metaCmd].Fn != nil {
			return nil, fmt.Errorf("invalid /%s subcommand '%s'", metaCmd, pk.MetaSubCmd)
		}
		return nil, fmt.Errorf("invalid command '/%s', no handler", cmdName)
	}
	return entry.Fn(ctx, pk)
}

func firstArg(pk *scpacket.FeCommandPacketType) string {
	if len(pk.Args) == 0 {
		return ""
	}
	return pk.Args[0]
}

func argN(pk *scpacket.FeCommandPacketType, n int) string {
	if len(pk.Args) <= n {
		return ""
	}
	return pk.Args[n]
}

func resolveBool(arg string, def bool) bool {
	if arg == "" {
		return def
	}
	if arg == "0" || arg == "false" {
		return false
	}
	return true
}

func resolveFile(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	fileName := base.ExpandHomeDir(arg)
	if !strings.HasPrefix(fileName, "/") {
		return "", fmt.Errorf("must be absolute, cannot be a relative path")
	}
	fd, err := os.Open(fileName)
	if fd != nil {
		fd.Close()
	}
	if err != nil {
		return "", fmt.Errorf("cannot open file: %v", err)
	}
	return fileName, nil
}

func resolvePosInt(arg string, def int) (int, error) {
	if arg == "" {
		return def, nil
	}
	ival, err := strconv.Atoi(arg)
	if err != nil {
		return 0, err
	}
	if ival <= 0 {
		return 0, fmt.Errorf("must be greater than 0")
	}
	return ival, nil
}

func resolveNonNegInt(arg string, def int) (int, error) {
	if arg == "" {
		return def, nil
	}
	ival, err := strconv.Atoi(arg)
	if err != nil {
		return 0, err
	}
	if ival < 0 {
		return 0, fmt.Errorf("cannot be negative")
	}
	return ival, nil
}

func getUITermOpts(uiContext *scpacket.UIContextType) *packet.TermOpts {
	termOpts := &packet.TermOpts{Rows: shexec.DefaultTermRows, Cols: shexec.DefaultTermCols, Term: remote.DefaultTerm, MaxPtySize: shexec.DefaultMaxPtySize}
	if uiContext != nil && uiContext.TermOpts != nil {
		pkOpts := uiContext.TermOpts
		if pkOpts.Cols > 0 {
			termOpts.Cols = base.BoundInt(pkOpts.Cols, shexec.MinTermCols, shexec.MaxTermCols)
		}
		if pkOpts.MaxPtySize > 0 {
			termOpts.MaxPtySize = base.BoundInt64(pkOpts.MaxPtySize, shexec.MinMaxPtySize, shexec.MaxMaxPtySize)
		}
	}
	return termOpts
}

func RunCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window|R_RemoteConnected)
	if err != nil {
		return nil, fmt.Errorf("/run error: %w", err)
	}
	cmdStr := firstArg(pk)
	isRtnStateCmd := IsReturnStateCommand(cmdStr)
	runPacket := packet.MakeRunPacket()
	runPacket.ReqId = uuid.New().String()
	runPacket.CK = base.MakeCommandKey(ids.SessionId, scbase.GenSCUUID())
	// runPacket.State is set in remote.RunCommand()
	runPacket.UsePty = true
	runPacket.TermOpts = getUITermOpts(pk.UIContext)
	runPacket.Command = strings.TrimSpace(cmdStr)
	runPacket.ReturnState = resolveBool(pk.Kwargs["rtnstate"], isRtnStateCmd)
	cmd, callback, err := remote.RunCommand(ctx, ids.SessionId, ids.WindowId, ids.Remote.RemotePtr, runPacket)
	if callback != nil {
		defer callback()
	}
	if err != nil {
		return nil, err
	}
	update, err := addLineForCmd(ctx, "/run", true, ids, cmd)
	if err != nil {
		return nil, err
	}
	update.Interactive = pk.Interactive
	sstore.MainBus.SendUpdate(ids.SessionId, update)
	return nil, nil
}

func addToHistory(ctx context.Context, pk *scpacket.FeCommandPacketType, historyContext historyContextType, isMetaCmd bool, hadError bool) error {
	cmdStr := firstArg(pk)
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window)
	if err != nil {
		return err
	}
	hitem := &sstore.HistoryItemType{
		HistoryId: scbase.GenSCUUID(),
		Ts:        time.Now().UnixMilli(),
		UserId:    DefaultUserId,
		SessionId: ids.SessionId,
		ScreenId:  ids.ScreenId,
		WindowId:  ids.WindowId,
		LineId:    historyContext.LineId,
		HadError:  hadError,
		CmdId:     historyContext.CmdId,
		CmdStr:    cmdStr,
		IsMetaCmd: isMetaCmd,
	}
	if !isMetaCmd && historyContext.RemotePtr != nil {
		hitem.Remote = *historyContext.RemotePtr
	}
	err = sstore.InsertHistoryItem(ctx, hitem)
	if err != nil {
		return err
	}
	return nil
}

func EvalCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("usage: /eval [command], no command passed to eval")
	}
	var historyContext historyContextType
	ctxWithHistory := context.WithValue(ctx, historyContextKey, &historyContext)
	var update sstore.UpdatePacket
	newPk, rtnErr := EvalMetaCommand(ctxWithHistory, pk)
	if rtnErr == nil {
		update, rtnErr = HandleCommand(ctxWithHistory, newPk)
	}
	if !resolveBool(pk.Kwargs["nohist"], false) {
		err := addToHistory(ctx, pk, historyContext, (newPk.MetaCmd != "run"), (rtnErr != nil))
		if err != nil {
			fmt.Printf("[error] adding to history: %v\n", err)
			// continue...
		}
	}
	return update, rtnErr
}

func ScreenCloseCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, fmt.Errorf("/screen:close cannot close screen: %w", err)
	}
	update, err := sstore.DeleteScreen(ctx, ids.SessionId, ids.ScreenId)
	if err != nil {
		return nil, err
	}
	return update, nil
}

func ScreenOpenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, fmt.Errorf("/screen:open cannot open screen: %w", err)
	}
	activate := resolveBool(pk.Kwargs["activate"], true)
	newName := pk.Kwargs["name"]
	if newName != "" {
		err := validateName(newName, "screen")
		if err != nil {
			return nil, err
		}
	}
	update, err := sstore.InsertScreen(ctx, ids.SessionId, newName, activate)
	if err != nil {
		return nil, err
	}
	return update, nil
}

func ScreenSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen)
	if err != nil {
		return nil, err
	}
	var varsUpdated []string
	if pk.Kwargs["name"] != "" {
		newName := pk.Kwargs["name"]
		err = validateName(newName, "screen")
		if err != nil {
			return nil, err
		}
		err = sstore.SetScreenName(ctx, ids.SessionId, ids.ScreenId, newName)
		if err != nil {
			return nil, fmt.Errorf("setting screen name: %v", err)
		}
		varsUpdated = append(varsUpdated, "name")
	}
	if pk.Kwargs["tabcolor"] != "" {
		color := pk.Kwargs["tabcolor"]
		err = validateColor(color, "screen tabcolor")
		if err != nil {
			return nil, err
		}
		screenObj, err := sstore.GetScreenById(ctx, ids.SessionId, ids.ScreenId)
		if err != nil {
			return nil, err
		}
		opts := screenObj.ScreenOpts
		if opts == nil {
			opts = &sstore.ScreenOptsType{}
		}
		opts.TabColor = color
		err = sstore.SetScreenOpts(ctx, ids.SessionId, ids.ScreenId, opts)
		if err != nil {
			return nil, fmt.Errorf("setting screen opts: %v", err)
		}
		varsUpdated = append(varsUpdated, "tabcolor")
	}
	if len(varsUpdated) == 0 {
		return nil, fmt.Errorf("/screen:set no updates, can set %s", formatStrs([]string{"name", "pos", "tabcolor"}, "or", false))
	}
	screenObj, err := sstore.GetScreenById(ctx, ids.SessionId, ids.ScreenId)
	if err != nil {
		return nil, err
	}
	update, session := sstore.MakeSingleSessionUpdate(ids.SessionId)
	session.Screens = append(session.Screens, screenObj)
	update.Info = &sstore.InfoMsgType{
		InfoMsg:   fmt.Sprintf("screen updated %s", formatStrs(varsUpdated, "and", false)),
		TimeoutMs: 2000,
	}
	return update, nil
}

func ScreenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, fmt.Errorf("/screen cannot switch to screen: %w", err)
	}
	firstArg := firstArg(pk)
	if firstArg == "" {
		return nil, fmt.Errorf("usage /screen [screen-name|screen-index|screen-id], no param specified")
	}
	ritem, err := resolveSessionScreen(ctx, ids.SessionId, firstArg, ids.ScreenId)
	if err != nil {
		return nil, err
	}
	update, err := sstore.SwitchScreenById(ctx, ids.SessionId, ritem.Id)
	if err != nil {
		return nil, err
	}
	return update, nil
}

var swAnchorRe = regexp.MustCompile("^(\\d+)(?::(-?\\d+))?$")

func SwSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window)
	if err != nil {
		return nil, fmt.Errorf("/sw:set cannot resolve current screen-window: %w", err)
	}
	var setNonST bool // scrolltop does not receive an update
	updateMap := make(map[string]interface{})
	if pk.Kwargs["anchor"] != "" {
		m := swAnchorRe.FindStringSubmatch(pk.Kwargs["anchor"])
		if m == nil {
			return nil, fmt.Errorf("/sw:set invalid anchor argument (must be [line] or [line]:[offset])")
		}
		anchorLine, _ := strconv.Atoi(m[1])
		updateMap[sstore.SWField_AnchorLine] = anchorLine
		if m[2] != "" {
			anchorOffset, _ := strconv.Atoi(m[2])
			updateMap[sstore.SWField_AnchorOffset] = anchorOffset
		}
	}
	if pk.Kwargs["focus"] != "" {
		focusVal := pk.Kwargs["focus"]
		if focusVal != sstore.SWFocusInput && focusVal != sstore.SWFocusCmd && focusVal != sstore.SWFocusCmdFg {
			return nil, fmt.Errorf("/sw:set invalid focus argument %q, must be %s", focusVal, formatStrs([]string{sstore.SWFocusInput, sstore.SWFocusCmd, sstore.SWFocusCmdFg}, "or", false))
		}
		updateMap[sstore.SWField_Focus] = focusVal
		setNonST = true
	}
	if pk.Kwargs["line"] != "" {
		sw, err := sstore.GetScreenWindowByIds(ctx, ids.SessionId, ids.ScreenId, ids.WindowId)
		if err != nil {
			return nil, fmt.Errorf("/sw:set cannot get screen-window: %v", err)
		}
		var selectedLineStr string
		if sw.SelectedLine > 0 {
			selectedLineStr = strconv.Itoa(sw.SelectedLine)
		}
		ritem, err := resolveLine(ctx, ids.SessionId, ids.WindowId, pk.Kwargs["line"], selectedLineStr)
		if err != nil {
			return nil, fmt.Errorf("/sw:set error resolving line: %v", err)
		}
		if ritem == nil {
			return nil, fmt.Errorf("/sw:set could not resolve line %q", pk.Kwargs["line"])
		}
		setNonST = true
		updateMap[sstore.SWField_SelectedLine] = ritem.Num
	}
	if len(updateMap) == 0 {
		return nil, fmt.Errorf("/sw:set no updates, can set %s", formatStrs([]string{"line", "scrolltop", "focus"}, "or", false))
	}
	sw, err := sstore.UpdateScreenWindow(ctx, ids.SessionId, ids.ScreenId, ids.WindowId, updateMap)
	if err != nil {
		return nil, fmt.Errorf("/sw:set failed to update: %v", err)
	}
	if !setNonST {
		return nil, nil
	}
	return sstore.ModelUpdate{ScreenWindows: []*sstore.ScreenWindowType{sw}}, nil
}

func RemoteInstallCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	mshell := ids.Remote.MShell
	go mshell.RunInstall()
	return sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func RemoteInstallCancelCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	mshell := ids.Remote.MShell
	go mshell.CancelInstall()
	return sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func RemoteConnectCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	go ids.Remote.MShell.Launch()
	return sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func RemoteDisconnectCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	force := resolveBool(pk.Kwargs["force"], false)
	go ids.Remote.MShell.Disconnect(force)
	return sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			PtyRemoteId: ids.Remote.RemotePtr.RemoteId,
		},
	}, nil
}

func makeRemoteEditUpdate_new(err error) sstore.UpdatePacket {
	redit := &sstore.RemoteEditType{
		RemoteEdit: true,
	}
	if err != nil {
		redit.ErrorStr = err.Error()
	}
	update := sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			RemoteEdit: redit,
		},
	}
	return update
}

func makeRemoteEditErrorReturn_new(visual bool, err error) (sstore.UpdatePacket, error) {
	if visual {
		return makeRemoteEditUpdate_new(err), nil
	}
	return nil, err
}

func makeRemoteEditUpdate_edit(ids resolvedIds, err error) sstore.UpdatePacket {
	redit := &sstore.RemoteEditType{
		RemoteEdit: true,
	}
	redit.RemoteId = ids.Remote.RemotePtr.RemoteId
	if ids.Remote.RemoteCopy.SSHOpts != nil {
		redit.KeyStr = ids.Remote.RemoteCopy.SSHOpts.SSHIdentity
		redit.HasPassword = (ids.Remote.RemoteCopy.SSHOpts.SSHPassword != "")
	}
	if err != nil {
		redit.ErrorStr = err.Error()
	}
	update := sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			RemoteEdit: redit,
		},
	}
	return update
}

func makeRemoteEditErrorReturn_edit(ids resolvedIds, visual bool, err error) (sstore.UpdatePacket, error) {
	if visual {
		return makeRemoteEditUpdate_edit(ids, err), nil
	}
	return nil, err
}

type RemoteEditArgs struct {
	CanonicalName string
	SSHOpts       *sstore.SSHOpts
	Sudo          bool
	ConnectMode   string
	Alias         string
	AutoInstall   bool
	SSHPassword   string
	SSHKeyFile    string
	Color         string
	EditMap       map[string]interface{}
}

func parseRemoteEditArgs(isNew bool, pk *scpacket.FeCommandPacketType) (*RemoteEditArgs, error) {
	var canonicalName string
	var sshOpts *sstore.SSHOpts
	var isSudo bool

	if isNew {
		if len(pk.Args) == 0 {
			return nil, fmt.Errorf("/remote:new must specify user@host argument (set visual=1 to edit in UI)")
		}
		userHost := pk.Args[0]
		m := userHostRe.FindStringSubmatch(userHost)
		if m == nil {
			return nil, fmt.Errorf("invalid format of user@host argument")
		}
		sudoStr, remoteUser, remoteHost, remotePortStr := m[1], m[2], m[3], m[4]
		var uhPort int
		if remotePortStr != "" {
			var err error
			uhPort, err = strconv.Atoi(remotePortStr)
			if err != nil {
				return nil, fmt.Errorf("invalid port specified on user@host argument")
			}
		}
		if sudoStr != "" {
			isSudo = true
		}
		if pk.Kwargs["sudo"] != "" {
			sudoArg := resolveBool(pk.Kwargs["sudo"], false)
			if isSudo && !sudoArg {
				return nil, fmt.Errorf("invalid 'sudo' argument, with sudo kw arg set to false")
			}
			if !isSudo && sudoArg {
				isSudo = true
			}
		}
		sshOpts = &sstore.SSHOpts{
			Local:   false,
			SSHHost: remoteHost,
			SSHUser: remoteUser,
		}
		portVal, err := resolvePosInt(pk.Kwargs["port"], 0)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q: %v", pk.Kwargs["port"], err)
		}
		if portVal != 0 && uhPort != 0 && portVal != uhPort {
			return nil, fmt.Errorf("invalid port argument, does not match port specified in 'user@host:port' argument")
		}
		if portVal == 0 && uhPort != 0 {
			portVal = uhPort
		}
		sshOpts.SSHPort = portVal
		canonicalName = remoteUser + "@" + remoteHost
		if isSudo {
			canonicalName = "sudo@" + canonicalName
		}
	} else {
		if pk.Kwargs["sudo"] != "" {
			return nil, fmt.Errorf("cannot update 'sudo' value")
		}
		if pk.Kwargs["port"] != "" {
			return nil, fmt.Errorf("cannot update 'port' value")
		}
	}
	alias := pk.Kwargs["alias"]
	if alias != "" {
		if len(alias) > MaxRemoteAliasLen {
			return nil, fmt.Errorf("alias too long, max length = %d", MaxRemoteAliasLen)
		}
		if !remoteAliasRe.MatchString(alias) {
			return nil, fmt.Errorf("invalid alias format")
		}
	}
	var connectMode string
	if isNew {
		connectMode = sstore.ConnectModeAuto
	}
	if pk.Kwargs["connectmode"] != "" {
		connectMode = pk.Kwargs["connectmode"]
	}
	if connectMode != "" && !sstore.IsValidConnectMode(connectMode) {
		err := fmt.Errorf("invalid connectmode %q: valid modes are %s", connectMode, formatStrs([]string{sstore.ConnectModeStartup, sstore.ConnectModeAuto, sstore.ConnectModeManual}, "or", false))
		return nil, err
	}
	autoInstall := resolveBool(pk.Kwargs["autoinstall"], true)
	keyFile, err := resolveFile(pk.Kwargs["key"])
	if err != nil {
		return nil, fmt.Errorf("invalid ssh keyfile %q: %v", pk.Kwargs["key"], err)
	}
	color := pk.Kwargs["color"]
	if color != "" {
		err := validateRemoteColor(color, "remote color")
		if err != nil {
			return nil, err
		}
	}
	sshPassword := pk.Kwargs["password"]
	if sshOpts != nil {
		sshOpts.SSHIdentity = keyFile
		sshOpts.SSHPassword = sshPassword
	}

	// set up editmap
	editMap := make(map[string]interface{})
	if _, found := pk.Kwargs[sstore.RemoteField_Alias]; found {
		editMap[sstore.RemoteField_Alias] = alias
	}
	if connectMode != "" {
		editMap[sstore.RemoteField_ConnectMode] = connectMode
	}
	if _, found := pk.Kwargs[sstore.RemoteField_AutoInstall]; found {
		editMap[sstore.RemoteField_AutoInstall] = autoInstall
	}
	if _, found := pk.Kwargs["key"]; found {
		editMap[sstore.RemoteField_SSHKey] = keyFile
	}
	if _, found := pk.Kwargs[sstore.RemoteField_Color]; found {
		editMap[sstore.RemoteField_Color] = color
	}
	if _, found := pk.Kwargs["password"]; found && pk.Kwargs["password"] != PasswordUnchangedSentinel {
		editMap[sstore.RemoteField_SSHPassword] = sshPassword
	}

	return &RemoteEditArgs{
		SSHOpts:       sshOpts,
		Sudo:          isSudo,
		ConnectMode:   connectMode,
		Alias:         alias,
		AutoInstall:   autoInstall,
		CanonicalName: canonicalName,
		SSHKeyFile:    keyFile,
		SSHPassword:   sshPassword,
		Color:         color,
		EditMap:       editMap,
	}, nil
}

func RemoteNewCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	visualEdit := resolveBool(pk.Kwargs["visual"], false)
	isSubmitted := resolveBool(pk.Kwargs["submit"], false)
	if visualEdit && !isSubmitted && len(pk.Args) == 0 {
		return makeRemoteEditUpdate_new(nil), nil
	}
	editArgs, err := parseRemoteEditArgs(true, pk)
	if err != nil {
		return makeRemoteEditErrorReturn_new(visualEdit, fmt.Errorf("/remote:new %v", err))
	}
	r := &sstore.RemoteType{
		RemoteId:            scbase.GenSCUUID(),
		PhysicalId:          "",
		RemoteType:          sstore.RemoteTypeSsh,
		RemoteAlias:         editArgs.Alias,
		RemoteCanonicalName: editArgs.CanonicalName,
		RemoteSudo:          editArgs.Sudo,
		RemoteUser:          editArgs.SSHOpts.SSHUser,
		RemoteHost:          editArgs.SSHOpts.SSHHost,
		ConnectMode:         editArgs.ConnectMode,
		AutoInstall:         editArgs.AutoInstall,
		SSHOpts:             editArgs.SSHOpts,
	}
	if editArgs.Color != "" {
		r.RemoteOpts = &sstore.RemoteOptsType{Color: editArgs.Color}
	}
	err = remote.AddRemote(ctx, r)
	if err != nil {
		return makeRemoteEditErrorReturn_new(visualEdit, fmt.Errorf("cannot create remote %q: %v", r.RemoteCanonicalName, err))
	}
	// SUCCESS
	update := sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("remote %q created", r.RemoteCanonicalName),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func RemoteSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	visualEdit := resolveBool(pk.Kwargs["visual"], false)
	isSubmitted := resolveBool(pk.Kwargs["submit"], false)
	editArgs, err := parseRemoteEditArgs(false, pk)
	if err != nil {
		return makeRemoteEditErrorReturn_edit(ids, visualEdit, fmt.Errorf("/remote:new %v", err))
	}
	if visualEdit && !isSubmitted && len(editArgs.EditMap) == 0 {
		return makeRemoteEditUpdate_edit(ids, nil), nil
	}
	if !visualEdit && len(editArgs.EditMap) == 0 {
		return nil, fmt.Errorf("/remote:set no updates, can set %s.  (set visual=1 to edit in UI)", formatStrs(RemoteSetArgs, "or", false))
	}
	err = ids.Remote.MShell.UpdateRemote(ctx, editArgs.EditMap)
	if err != nil {
		return makeRemoteEditErrorReturn_edit(ids, visualEdit, fmt.Errorf("/remote:new error updating remote: %v", err))
	}
	update := sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("remote %q updated", ids.Remote.DisplayName),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func RemoteShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	state := ids.Remote.RState
	return sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			PtyRemoteId: state.RemoteId,
		},
	}, nil
}

func RemoteShowAllCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	stateArr := remote.GetAllRemoteRuntimeState()
	var buf bytes.Buffer
	for _, rstate := range stateArr {
		var name string
		if rstate.RemoteAlias == "" {
			name = rstate.RemoteCanonicalName
		} else {
			name = fmt.Sprintf("%s (%s)", rstate.RemoteCanonicalName, rstate.RemoteAlias)
		}
		buf.WriteString(fmt.Sprintf("%-12s %-5s %8s  %s\n", rstate.Status, rstate.RemoteType, rstate.RemoteId[0:8], name))
	}
	return sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			RemoteShowAll: true,
		},
	}, nil
}

func RemoteArchiveCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	err = remote.ArchiveRemote(ctx, ids.Remote.RemotePtr.RemoteId)
	if err != nil {
		return nil, fmt.Errorf("archiving remote: %v", err)
	}
	update := sstore.InfoMsgUpdate("remote [%s] archived", ids.Remote.DisplayName)
	localRemote := remote.GetLocalRemote()
	if localRemote != nil {
		update.Window = &sstore.WindowType{
			SessionId: ids.SessionId,
			WindowId:  ids.WindowId,
			CurRemote: sstore.RemotePtrType{RemoteId: localRemote.GetRemoteId()},
		}
	}
	return update, nil
}

func RemoteCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("/remote requires a subcommand: %s", formatStrs([]string{"show"}, "or", false))
}

func CrCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window)
	if err != nil {
		return nil, fmt.Errorf("/cr error: %w", err)
	}
	newRemote := firstArg(pk)
	if newRemote == "" {
		return nil, nil
	}
	remoteName, rptr, _, rstate, err := resolveRemote(ctx, newRemote, ids.SessionId, ids.WindowId)
	if err != nil {
		return nil, err
	}
	if rptr == nil {
		return nil, fmt.Errorf("/cr error: remote %q not found", newRemote)
	}
	if rstate.Archived {
		return nil, fmt.Errorf("/cr error: remote %q cannot switch to archived remote", newRemote)
	}
	err = sstore.UpdateCurRemote(ctx, ids.SessionId, ids.WindowId, *rptr)
	if err != nil {
		return nil, fmt.Errorf("/cr error: cannot update curremote: %w", err)
	}
	update := sstore.ModelUpdate{
		Window: &sstore.WindowType{
			SessionId: ids.SessionId,
			WindowId:  ids.WindowId,
			CurRemote: *rptr,
		},
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("current remote = %q", remoteName),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func CdCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_RemoteConnected)
	if err != nil {
		return nil, fmt.Errorf("/cd error: %w", err)
	}
	newDir := firstArg(pk)
	if newDir == "" {
		return sstore.ModelUpdate{
			Info: &sstore.InfoMsgType{
				InfoMsg: fmt.Sprintf("[%s] current directory = %s", ids.Remote.DisplayName, ids.Remote.RemoteState.Cwd),
			},
		}, nil
	}
	newDir, err = ids.Remote.RState.ExpandHomeDir(newDir)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(newDir, "/") {
		if ids.Remote.RemoteState == nil {
			return nil, fmt.Errorf("/cd error: cannot get current remote directory (can only cd with absolute path)")
		}
		newDir = path.Join(ids.Remote.RemoteState.Cwd, newDir)
		newDir, err = filepath.Abs(newDir)
		if err != nil {
			return nil, fmt.Errorf("/cd error: error canonicalizing new directory: %w", err)
		}
	}
	cdPacket := packet.MakeCdPacket()
	cdPacket.ReqId = uuid.New().String()
	cdPacket.Dir = newDir
	resp, err := ids.Remote.MShell.PacketRpc(ctx, cdPacket)
	if err != nil {
		return nil, err
	}
	if err = resp.Err(); err != nil {
		return nil, err
	}
	state := *ids.Remote.RemoteState
	state.Cwd = newDir
	remoteInst, err := sstore.UpdateRemoteState(ctx, ids.SessionId, ids.WindowId, ids.Remote.RemotePtr, state)
	if err != nil {
		return nil, err
	}
	var cmdOutput bytes.Buffer
	displayStateUpdate(&cmdOutput, *ids.Remote.RemoteState, remoteInst.State)
	cmd, err := makeStaticCmd(ctx, "cd", ids, pk.GetRawStr(), cmdOutput.Bytes())
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update, err := addLineForCmd(ctx, "/cd", false, ids, cmd)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	update.Sessions = sstore.MakeSessionsUpdateForRemote(ids.SessionId, remoteInst)
	//update.Info = &sstore.InfoMsgType{
	//	InfoMsg:   fmt.Sprintf("[%s] current directory = %s", ids.Remote.DisplayName, newDir),
	//	TimeoutMs: 2000,
	//}
	return update, nil
}

func makeStaticCmd(ctx context.Context, metaCmd string, ids resolvedIds, cmdStr string, cmdOutput []byte) (*sstore.CmdType, error) {
	cmd := &sstore.CmdType{
		SessionId: ids.SessionId,
		CmdId:     scbase.GenSCUUID(),
		CmdStr:    cmdStr,
		Remote:    ids.Remote.RemotePtr,
		TermOpts:  sstore.TermOpts{Rows: shexec.DefaultTermRows, Cols: shexec.DefaultTermCols, FlexRows: true, MaxPtySize: remote.DefaultMaxPtySize},
		Status:    sstore.CmdStatusDone,
		StartPk:   nil,
		DonePk:    nil,
		RunOut:    nil,
	}
	if ids.Remote.RemoteState != nil {
		cmd.RemoteState = *ids.Remote.RemoteState
	}
	err := sstore.CreateCmdPtyFile(ctx, cmd.SessionId, cmd.CmdId, cmd.TermOpts.MaxPtySize)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, fmt.Errorf("cannot create local ptyout file for %s command: %w", metaCmd, err)
	}
	// can ignore ptyupdate
	_, err = sstore.AppendToCmdPtyBlob(ctx, cmd.SessionId, cmd.CmdId, cmdOutput, 0)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, fmt.Errorf("cannot append to local ptyout file for %s command: %v", metaCmd, err)
	}
	return cmd, nil
}

func addLineForCmd(ctx context.Context, metaCmd string, shouldFocus bool, ids resolvedIds, cmd *sstore.CmdType) (*sstore.ModelUpdate, error) {
	rtnLine, err := sstore.AddCmdLine(ctx, ids.SessionId, ids.WindowId, DefaultUserId, cmd)
	if err != nil {
		return nil, err
	}
	sw, err := sstore.GetScreenWindowByIds(ctx, ids.SessionId, ids.ScreenId, ids.WindowId)
	if err != nil {
		// ignore error here, because the command has already run (nothing to do)
		fmt.Printf("%s error getting screen-window: %v\n", metaCmd, err)
	}
	if sw != nil {
		updateMap := make(map[string]interface{})
		updateMap[sstore.SWField_SelectedLine] = rtnLine.LineNum
		if shouldFocus {
			updateMap[sstore.SWField_Focus] = sstore.SWFocusCmdFg
		}
		sw, err = sstore.UpdateScreenWindow(ctx, ids.SessionId, ids.ScreenId, ids.WindowId, updateMap)
		if err != nil {
			// ignore error again (nothing to do)
			fmt.Printf("%s error updating screen-window selected line: %v\n", metaCmd, err)
		}
	}
	update := &sstore.ModelUpdate{
		Line:          rtnLine,
		Cmd:           cmd,
		ScreenWindows: []*sstore.ScreenWindowType{sw},
	}
	updateHistoryContext(ctx, rtnLine, cmd)
	return update, nil
}

func updateHistoryContext(ctx context.Context, line *sstore.LineType, cmd *sstore.CmdType) {
	ctxVal := ctx.Value(historyContextKey)
	if ctxVal == nil {
		return
	}
	hctx := ctxVal.(*historyContextType)
	if line != nil {
		hctx.LineId = line.LineId
	}
	if cmd != nil {
		hctx.CmdId = cmd.CmdId
		hctx.RemotePtr = &cmd.Remote
	}
}

func getStrArr(v interface{}, field string) []string {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	fieldVal := m[field]
	if fieldVal == nil {
		return nil
	}
	iarr, ok := fieldVal.([]interface{})
	if !ok {
		return nil
	}
	var sarr []string
	for _, iv := range iarr {
		if sv, ok := iv.(string); ok {
			sarr = append(sarr, sv)
		}
	}
	return sarr
}

func getBool(v interface{}, field string) bool {
	if v == nil {
		return false
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	fieldVal := m[field]
	if fieldVal == nil {
		return false
	}
	bval, ok := fieldVal.(bool)
	if !ok {
		return false
	}
	return bval
}

func makeInfoFromComps(compType string, comps []string, hasMore bool) sstore.UpdatePacket {
	sort.Slice(comps, func(i int, j int) bool {
		c1 := comps[i]
		c2 := comps[j]
		c1mc := strings.HasPrefix(c1, "^")
		c2mc := strings.HasPrefix(c2, "^")
		if c1mc && !c2mc {
			return true
		}
		if !c1mc && c2mc {
			return false
		}
		return c1 < c2
	})
	if len(comps) == 0 {
		comps = []string{"(no completions)"}
	}
	update := sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle:     fmt.Sprintf("%s completions", compType),
			InfoComps:     comps,
			InfoCompsMore: hasMore,
		},
	}
	return update
}

func makeInsertUpdateFromComps(pos int64, prefix string, comps []string, hasMore bool) sstore.UpdatePacket {
	if hasMore {
		return nil
	}
	lcp := longestPrefix(prefix, comps)
	if lcp == prefix || len(lcp) < len(prefix) || !strings.HasPrefix(lcp, prefix) {
		return nil
	}
	insertChars := lcp[len(prefix):]
	clu := &sstore.CmdLineType{InsertChars: insertChars, InsertPos: pos}
	return sstore.ModelUpdate{CmdLine: clu}
}

func longestPrefix(root string, comps []string) string {
	if len(comps) == 0 {
		return root
	}
	if len(comps) == 1 {
		comp := comps[0]
		if len(comp) >= len(root) && strings.HasPrefix(comp, root) {
			if strings.HasSuffix(comp, "/") {
				return comps[0]
			}
			return comps[0] + " "
		}
	}
	lcp := comps[0]
	for i := 1; i < len(comps); i++ {
		s := comps[i]
		for j := 0; j < len(lcp); j++ {
			if j >= len(s) || lcp[j] != s[j] {
				lcp = lcp[0:j]
				break
			}
		}
	}
	if len(lcp) < len(root) || !strings.HasPrefix(lcp, root) {
		return root
	}
	return lcp
}

func doMetaCompGen(ctx context.Context, pk *scpacket.FeCommandPacketType, prefix string, forDisplay bool) ([]string, bool, error) {
	ids, err := resolveUiIds(ctx, pk, 0) // best effort
	var comps []string
	var hasMore bool
	if ids.Remote != nil && ids.Remote.RState.IsConnected() {
		comps, hasMore, err = doCompGen(ctx, pk, prefix, "file", forDisplay)
		if err != nil {
			return nil, false, err
		}
	}
	validCommands := getValidCommands()
	for _, cmd := range validCommands {
		if strings.HasPrefix(cmd, prefix) {
			if forDisplay {
				comps = append(comps, "^"+cmd)
			} else {
				comps = append(comps, cmd)
			}
		}
	}
	return comps, hasMore, nil
}

func doCompGen(ctx context.Context, pk *scpacket.FeCommandPacketType, prefix string, compType string, forDisplay bool) ([]string, bool, error) {
	if compType == "metacommand" {
		return doMetaCompGen(ctx, pk, prefix, forDisplay)
	}
	if !packet.IsValidCompGenType(compType) {
		return nil, false, fmt.Errorf("/compgen invalid type '%s'", compType)
	}
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window|R_RemoteConnected)
	if err != nil {
		return nil, false, fmt.Errorf("compgen error: %w", err)
	}
	cgPacket := packet.MakeCompGenPacket()
	cgPacket.ReqId = uuid.New().String()
	cgPacket.CompType = compType
	cgPacket.Prefix = prefix
	cgPacket.Cwd = ids.Remote.RemoteState.Cwd
	resp, err := ids.Remote.MShell.PacketRpc(ctx, cgPacket)
	if err != nil {
		return nil, false, err
	}
	if err = resp.Err(); err != nil {
		return nil, false, err
	}
	comps := getStrArr(resp.Data, "comps")
	hasMore := getBool(resp.Data, "hasmore")
	return comps, hasMore, nil
}

func CompGenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	cmdLine := firstArg(pk)
	pos := len(cmdLine)
	if pk.Kwargs["comppos"] != "" {
		posArg, err := strconv.Atoi(pk.Kwargs["comppos"])
		if err != nil {
			return nil, fmt.Errorf("/compgen invalid comppos '%s': %w", pk.Kwargs["comppos"], err)
		}
		pos = posArg
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(cmdLine) {
		pos = len(cmdLine)
	}
	showComps := resolveBool(pk.Kwargs["compshow"], false)
	prefix := cmdLine[:pos]
	parts := strings.Split(prefix, " ")
	compType := "file"
	if len(parts) > 0 && len(parts) < 2 && strings.HasPrefix(parts[0], "/") {
		compType = "metacommand"
	} else if len(parts) == 2 && (parts[0] == "cd" || parts[0] == "/cd") {
		compType = "directory"
	} else if len(parts) <= 1 {
		compType = "command"
	}
	lastPart := ""
	if len(parts) > 0 {
		lastPart = parts[len(parts)-1]
	}
	comps, hasMore, err := doCompGen(ctx, pk, lastPart, compType, showComps)
	if err != nil {
		return nil, err
	}
	if showComps {
		return makeInfoFromComps(compType, comps, hasMore), nil
	}
	return makeInsertUpdateFromComps(int64(pos), lastPart, comps, hasMore), nil
}

func CommentCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Window)
	if err != nil {
		return nil, fmt.Errorf("/comment error: %w", err)
	}
	text := firstArg(pk)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("cannot post empty comment")
	}
	rtnLine, err := sstore.AddCommentLine(ctx, ids.SessionId, ids.WindowId, DefaultUserId, text)
	if err != nil {
		return nil, err
	}
	updateMap := make(map[string]interface{})
	updateMap[sstore.SWField_SelectedLine] = rtnLine.LineNum
	updateMap[sstore.SWField_Focus] = sstore.SWFocusInput
	sw, err := sstore.UpdateScreenWindow(ctx, ids.SessionId, ids.ScreenId, ids.WindowId, updateMap)
	if err != nil {
		// ignore error again (nothing to do)
		fmt.Printf("/comment error updating screen-window selected line: %v\n", err)
	}
	update := sstore.ModelUpdate{Line: rtnLine, ScreenWindows: []*sstore.ScreenWindowType{sw}}
	return update, nil
}

func maybeQuote(s string, quote bool) string {
	if quote {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func mapToStrs(m map[string]bool) []string {
	var rtn []string
	for key, val := range m {
		if val {
			rtn = append(rtn, key)
		}
	}
	return rtn
}

func formatStrs(strs []string, conj string, quote bool) string {
	if len(strs) == 0 {
		return "(none)"
	}
	if len(strs) == 1 {
		return maybeQuote(strs[0], quote)
	}
	if len(strs) == 2 {
		return fmt.Sprintf("%s %s %s", maybeQuote(strs[0], quote), conj, maybeQuote(strs[1], quote))
	}
	var buf bytes.Buffer
	for idx := 0; idx < len(strs)-1; idx++ {
		buf.WriteString(maybeQuote(strs[idx], quote))
		buf.WriteString(", ")
	}
	buf.WriteString(conj)
	buf.WriteString(" ")
	buf.WriteString(maybeQuote(strs[len(strs)-1], quote))
	return buf.String()
}

func validateName(name string, typeStr string) error {
	if len(name) > MaxNameLen {
		return fmt.Errorf("%s name too long, max length is %d", typeStr, MaxNameLen)
	}
	if !genericNameRe.MatchString(name) {
		return fmt.Errorf("invalid %s name", typeStr)
	}
	return nil
}

func validateColor(color string, typeStr string) error {
	for _, c := range ColorNames {
		if color == c {
			return nil
		}
	}
	return fmt.Errorf("invalid %s, valid colors are: %s", typeStr, formatStrs(ColorNames, "or", false))
}

func validateRemoteColor(color string, typeStr string) error {
	for _, c := range RemoteColorNames {
		if color == c {
			return nil
		}
	}
	return fmt.Errorf("invalid %s, valid colors are: %s", typeStr, formatStrs(RemoteColorNames, "or", false))
}

func SessionOpenCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	activate := resolveBool(pk.Kwargs["activate"], true)
	newName := pk.Kwargs["name"]
	if newName != "" {
		err := validateName(newName, "session")
		if err != nil {
			return nil, err
		}
	}
	update, err := sstore.InsertSessionWithName(ctx, newName, activate)
	if err != nil {
		return nil, err
	}
	return update, nil
}

func SessionDeleteCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, err
	}
	err = sstore.DeleteSession(ctx, ids.SessionId)
	if err != nil {
		return nil, fmt.Errorf("cannot delete session: %v", err)
	}
	sessionIds, _ := sstore.GetAllSessionIds(ctx) // ignore error, session is already deleted so that's the main return value
	delSession := &sstore.SessionType{SessionId: ids.SessionId, Remove: true}
	update := sstore.ModelUpdate{
		Sessions: []*sstore.SessionType{delSession},
	}
	if len(sessionIds) > 0 {
		update.ActiveSessionId = sessionIds[0]
	}
	return update, nil
}

func SessionSetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session)
	if err != nil {
		return nil, err
	}
	var varsUpdated []string
	if pk.Kwargs["name"] != "" {
		newName := pk.Kwargs["name"]
		err = validateName(newName, "session")
		if err != nil {
			return nil, err
		}
		err = sstore.SetSessionName(ctx, ids.SessionId, newName)
		if err != nil {
			return nil, fmt.Errorf("setting session name: %v", err)
		}
		varsUpdated = append(varsUpdated, "name")
	}
	if pk.Kwargs["pos"] != "" {

	}
	if len(varsUpdated) == 0 {
		return nil, fmt.Errorf("/session:set no updates, can set %s", formatStrs([]string{"name", "pos"}, "or", false))
	}
	bareSession, err := sstore.GetBareSessionById(ctx, ids.SessionId)
	update := sstore.ModelUpdate{
		Sessions: []*sstore.SessionType{bareSession},
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("session updated %s", formatStrs(varsUpdated, "and", false)),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func SessionCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, 0)
	if err != nil {
		return nil, err
	}
	firstArg := firstArg(pk)
	if firstArg == "" {
		return nil, fmt.Errorf("usage /session [name|id|pos], no param specified")
	}
	bareSessions, err := sstore.GetBareSessions(ctx)
	if err != nil {
		return nil, err
	}
	ritems := sessionsToResolveItems(bareSessions)
	ritem, err := genericResolve(firstArg, ids.SessionId, ritems, false, "session")
	if err != nil {
		return nil, err
	}
	err = sstore.SetActiveSessionId(ctx, ritem.Id)
	if err != nil {
		return nil, err
	}
	update := sstore.ModelUpdate{
		ActiveSessionId: ritem.Id,
		Info: &sstore.InfoMsgType{
			InfoMsg:   fmt.Sprintf("switched to session %q", ritem.Name),
			TimeoutMs: 2000,
		},
	}
	return update, nil
}

func ResetCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window)
	if err != nil {
		return nil, err
	}
	initPk, err := ids.Remote.MShell.ReInit(ctx)
	if err != nil {
		return nil, err
	}
	if initPk == nil || initPk.State == nil {
		return nil, fmt.Errorf("invalid initpk received from remote (no remote state)")
	}
	remoteInst, err := sstore.UpdateRemoteState(ctx, ids.SessionId, ids.WindowId, ids.Remote.RemotePtr, *initPk.State)
	if err != nil {
		return nil, err
	}
	outputStr := "reset remote state"
	cmd, err := makeStaticCmd(ctx, "reset", ids, pk.GetRawStr(), []byte(outputStr))
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update, err := addLineForCmd(ctx, "/cd", false, ids, cmd)
	if err != nil {
		// TODO tricky error since the command was a success, but we can't show the output
		return nil, err
	}
	update.Interactive = pk.Interactive
	update.Sessions = sstore.MakeSessionsUpdateForRemote(ids.SessionId, remoteInst)
	return update, nil
}

func ClearCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window)
	if err != nil {
		return nil, err
	}
	update, err := sstore.ClearWindow(ctx, ids.SessionId, ids.WindowId)
	if err != nil {
		return nil, fmt.Errorf("clearing window: %v", err)
	}
	update.Info = &sstore.InfoMsgType{
		InfoMsg:   fmt.Sprintf("window cleared"),
		TimeoutMs: 2000,
	}
	return update, nil
}

const DefaultMaxHistoryItems = 10000

func HistoryCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window|R_Remote)
	if err != nil {
		return nil, err
	}
	maxItems, err := resolvePosInt(pk.Kwargs["maxitems"], DefaultMaxHistoryItems)
	if err != nil {
		return nil, fmt.Errorf("invalid maxitems value '%s' (must be a number): %v", pk.Kwargs["maxitems"], err)
	}
	if maxItems < 0 {
		return nil, fmt.Errorf("invalid maxitems value '%d' (cannot be negative)", maxItems)
	}
	if maxItems == 0 {
		maxItems = DefaultMaxHistoryItems
	}
	htype := HistoryTypeWindow
	hSessionId := ids.SessionId
	hWindowId := ids.WindowId
	if pk.Kwargs["type"] != "" {
		htype = pk.Kwargs["type"]
		if htype != HistoryTypeWindow && htype != HistoryTypeSession && htype != HistoryTypeGlobal {
			return nil, fmt.Errorf("invalid history type '%s', valid types: %s", htype, formatStrs([]string{HistoryTypeWindow, HistoryTypeSession, HistoryTypeGlobal}, "or", false))
		}
	}
	if htype == HistoryTypeGlobal {
		hSessionId = ""
		hWindowId = ""
	} else if htype == HistoryTypeSession {
		hWindowId = ""
	}
	hitems, err := sstore.GetHistoryItems(ctx, hSessionId, hWindowId, sstore.HistoryQueryOpts{MaxItems: maxItems})
	if err != nil {
		return nil, err
	}
	show := !resolveBool(pk.Kwargs["noshow"], false)
	update := sstore.ModelUpdate{}
	update.History = &sstore.HistoryInfoType{
		HistoryType: htype,
		SessionId:   ids.SessionId,
		WindowId:    ids.WindowId,
		Items:       hitems,
		Show:        show,
	}
	return update, nil
}

func splitLinesForInfo(str string) []string {
	rtn := strings.Split(str, "\n")
	if rtn[len(rtn)-1] == "" {
		return rtn[:len(rtn)-1]
	}
	return rtn
}

func resizeRunningCommand(ctx context.Context, cmd *sstore.CmdType, newCols int) error {
	fmt.Printf("resize running cmd %s/%s %d => %d\n", cmd.SessionId, cmd.CmdId, cmd.TermOpts.Cols, newCols)
	siPk := packet.MakeSpecialInputPacket()
	siPk.CK = base.MakeCommandKey(cmd.SessionId, cmd.CmdId)
	siPk.WinSize = &packet.WinSize{Rows: int(cmd.TermOpts.Rows), Cols: newCols}
	msh := remote.GetRemoteById(cmd.Remote.RemoteId)
	if msh == nil {
		return fmt.Errorf("cannot resize, cmd remote not found")
	}
	err := msh.SendSpecialInput(siPk)
	if err != nil {
		return err
	}
	newTermOpts := cmd.TermOpts
	newTermOpts.Cols = int64(newCols)
	err = sstore.UpdateCmdTermOpts(ctx, cmd.SessionId, cmd.CmdId, newTermOpts)
	if err != nil {
		return err
	}
	return nil
}

func SwResizeCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window)
	if err != nil {
		return nil, err
	}
	colsStr := pk.Kwargs["cols"]
	if colsStr == "" {
		return nil, fmt.Errorf("/sw:resize requires a numeric 'cols' argument")
	}
	cols, err := strconv.Atoi(colsStr)
	if err != nil {
		return nil, fmt.Errorf("/sw:resize requires a numeric 'cols' argument: %v", err)
	}
	if cols <= 0 {
		return nil, fmt.Errorf("/sw:resize invalid zero/negative 'cols' argument")
	}
	cols = base.BoundInt(cols, shexec.MinTermCols, shexec.MaxTermCols)
	runningCmds, err := sstore.GetRunningWindowCmds(ctx, ids.SessionId, ids.WindowId)
	if err != nil {
		return nil, fmt.Errorf("/sw:resize cannot get running commands: %v", err)
	}
	if len(runningCmds) == 0 {
		return nil, nil
	}
	for _, cmd := range runningCmds {
		if int(cmd.TermOpts.Cols) != cols {
			resizeRunningCommand(ctx, cmd, cols)
		}
	}
	return nil, nil
}

func LineCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	return nil, fmt.Errorf("/line requires a subcommand: %s", formatStrs([]string{"show"}, "or", false))
}

func LineShowCommand(ctx context.Context, pk *scpacket.FeCommandPacketType) (sstore.UpdatePacket, error) {
	ids, err := resolveUiIds(ctx, pk, R_Session|R_Screen|R_Window)
	if err != nil {
		return nil, err
	}
	if len(pk.Args) == 0 {
		return nil, fmt.Errorf("/line:show requires an argument (line number or id)")
	}
	lineArg := pk.Args[0]
	lineId, err := sstore.FindLineIdByArg(ctx, ids.SessionId, ids.WindowId, lineArg)
	if err != nil {
		return nil, fmt.Errorf("error looking up lineid: %v", err)
	}
	if lineId == "" {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	line, cmd, err := sstore.GetLineCmdByLineId(ctx, ids.SessionId, ids.WindowId, lineId)
	if err != nil {
		return nil, fmt.Errorf("error getting line: %v", err)
	}
	if line == nil {
		return nil, fmt.Errorf("line %q not found", lineArg)
	}
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "lineid", line.LineId))
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "type", line.LineType))
	lineNumStr := strconv.FormatInt(line.LineNum, 10)
	if line.LineNumTemp {
		lineNumStr = "~" + lineNumStr
	}
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "linenum", lineNumStr))
	ts := time.UnixMilli(line.Ts)
	buf.WriteString(fmt.Sprintf("  %-15s %s\n", "ts", ts.Format("2006-01-02 15:04:05")))
	if line.Ephemeral {
		buf.WriteString(fmt.Sprintf("  %-15s %v\n", "ephemeral", true))
	}
	if cmd != nil {
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "cmdid", cmd.CmdId))
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "remote", cmd.Remote.MakeFullRemoteRef()))
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "status", cmd.Status))
		if cmd.RemoteState.Cwd != "" {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "cwd", cmd.RemoteState.Cwd))
		}
		buf.WriteString(fmt.Sprintf("  %-15s %s\n", "termopts", formatTermOpts(cmd.TermOpts)))
		if cmd.TermOpts != cmd.OrigTermOpts {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "orig-termopts", formatTermOpts(cmd.OrigTermOpts)))
		}
		if cmd.RtnState {
			buf.WriteString(fmt.Sprintf("  %-15s %s\n", "rtnstate", "true"))
		}
	}
	update := sstore.ModelUpdate{
		Info: &sstore.InfoMsgType{
			InfoTitle: fmt.Sprintf("line %d info", line.LineNum),
			InfoLines: splitLinesForInfo(buf.String()),
		},
	}
	return update, nil
}

func formatTermOpts(termOpts sstore.TermOpts) string {
	if termOpts.Cols == 0 {
		return "???"
	}
	rtnStr := fmt.Sprintf("%dx%d", termOpts.Rows, termOpts.Cols)
	if termOpts.FlexRows {
		rtnStr += " flexrows"
	}
	if termOpts.MaxPtySize > 0 {
		rtnStr += " maxbuf=" + scbase.NumFormatB2(termOpts.MaxPtySize)
	}
	return rtnStr
}

type ColMeta struct {
	Title   string
	MinCols int
	MaxCols int
}

func toInterfaceArr(sarr []string) []interface{} {
	rtn := make([]interface{}, len(sarr))
	for idx, s := range sarr {
		rtn[idx] = s
	}
	return rtn
}

func formatTextTable(totalCols int, data [][]string, colMeta []ColMeta) []string {
	numCols := len(colMeta)
	maxColLen := make([]int, len(colMeta))
	for i, cm := range colMeta {
		maxColLen[i] = cm.MinCols
	}
	for _, row := range data {
		for i := 0; i < numCols && i < len(row); i++ {
			dlen := len(row[i])
			if dlen > maxColLen[i] {
				maxColLen[i] = dlen
			}
		}
	}
	fmtStr := ""
	for idx, clen := range maxColLen {
		if idx != 0 {
			fmtStr += " "
		}
		fmtStr += fmt.Sprintf("%%%ds", clen)
	}
	var rtn []string
	for _, row := range data {
		sval := fmt.Sprintf(fmtStr, toInterfaceArr(row)...)
		rtn = append(rtn, sval)
	}
	return rtn
}

func displayStateUpdate(buf *bytes.Buffer, oldState packet.ShellState, newState packet.ShellState) {
	if newState.Cwd != oldState.Cwd {
		buf.WriteString(fmt.Sprintf("cwd %s\n", newState.Cwd))
	}
	if !bytes.Equal(newState.ShellVars, oldState.ShellVars) {
		newEnvMap := shexec.DeclMapFromState(&newState)
		oldEnvMap := shexec.DeclMapFromState(&oldState)
		for key, newVal := range newEnvMap {
			oldVal, found := oldEnvMap[key]
			if !found || ((oldVal.Value != newVal.Value) || (oldVal.IsExport() != newVal.IsExport())) {
				var exportStr string
				if newVal.IsExport() {
					exportStr = "export "
				}
				buf.WriteString(fmt.Sprintf("%s%s=%s\n", exportStr, key, ShellQuote(newVal.Value, false, 50)))
			}
		}
		for key, _ := range oldEnvMap {
			_, found := newEnvMap[key]
			if !found {
				buf.WriteString(fmt.Sprintf("unset %s\n", key))
			}
		}
	}
	if newState.Aliases != oldState.Aliases {
		newAliasMap, _ := ParseAliases(newState.Aliases)
		oldAliasMap, _ := ParseAliases(oldState.Aliases)
		for aliasName, newAliasVal := range newAliasMap {
			oldAliasVal, found := oldAliasMap[aliasName]
			if !found || newAliasVal != oldAliasVal {
				buf.WriteString(fmt.Sprintf("alias %s\n", shellescape.Quote(aliasName)))
			}
		}
		for aliasName, _ := range oldAliasMap {
			_, found := newAliasMap[aliasName]
			if !found {
				buf.WriteString(fmt.Sprintf("unalias %s\n", shellescape.Quote(aliasName)))
			}
		}
	}
	if newState.Funcs != oldState.Funcs {
		newFuncMap, _ := ParseFuncs(newState.Funcs)
		oldFuncMap, _ := ParseFuncs(oldState.Funcs)
		for funcName, newFuncVal := range newFuncMap {
			oldFuncVal, found := oldFuncMap[funcName]
			if !found || newFuncVal != oldFuncVal {
				buf.WriteString(fmt.Sprintf("function %s\n", shellescape.Quote(funcName)))
			}
		}
		for funcName, _ := range oldFuncMap {
			_, found := newFuncMap[funcName]
			if !found {
				buf.WriteString(fmt.Sprintf("unset -f %s\n", shellescape.Quote(funcName)))
			}
		}
	}
}

func GetRtnStateDiff(ctx context.Context, sessionId string, cmdId string) ([]byte, error) {
	cmd, err := sstore.GetCmdById(ctx, sessionId, cmdId)
	if err != nil {
		return nil, err
	}
	if cmd == nil {
		return nil, nil
	}
	if !cmd.RtnState {
		return nil, nil
	}
	if cmd.DonePk == nil || cmd.DonePk.FinalState == nil {
		return nil, nil
	}
	var outputBytes bytes.Buffer
	displayStateUpdate(&outputBytes, cmd.RemoteState, *cmd.DonePk.FinalState)
	return outputBytes.Bytes(), nil
}
