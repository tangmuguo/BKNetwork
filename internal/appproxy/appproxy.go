// Package appproxy installs per-user launch wrappers for applications that
// must continue to use a local Clash HTTP proxy while BKNetwork manages a
// separate WARP path for other traffic.
//
// The package deliberately keeps all state below the user's home directory.
// It never changes the desktop-wide proxy, system desktop files, or network
// state.  The generated wrappers contain the proxy environment and therefore
// do not depend on a particular BKNetwork binary remaining installed.
package appproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	configVersion = 1
	minPort       = 1
	maxPort       = 65535

	configRelativePath = ".local/share/bknetwork/app-proxy/config.json"
	journalName        = "journal.json"
	backupDirName      = "backup"
	wrapperDirName     = "bin"

	chatGPTWrapperName = "chatgpt-clash.sh"
	quotaWrapperName   = "quota-float-clash.sh"

	localApplicationsRelativePath = ".local/share/applications"
	autostartRelativePath         = ".config/autostart"

	maxDesktopFileBytes = 1024 * 1024
	maxJournalEntries   = 64
)

// Config is the small, stable enable marker read by the root BKNetwork
// service.  A file is considered enabled only when both fields are valid.
type Config struct {
	Version int `json:"version"`
	Port    int `json:"port"`
}

// ConfigPath returns the fixed per-user app-proxy manifest path.  It does not
// consult XDG variables because the root service needs to resolve a desktop
// user's home directory deterministically.
func ConfigPath(home string) string {
	return filepath.Join(home, filepath.FromSlash(configRelativePath))
}

// ReadConfig reads and validates the per-user enable marker.
//
// A missing marker is returned as os.ErrNotExist.  Invalid markers are
// returned as errors and never treated as an enabled configuration.
func ReadConfig(home string) (*Config, error) {
	path := ConfigPath(home)
	raw, err := readBoundedRegular(path, 16*1024)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("invalid app-proxy config: %w", err)
	}
	if !validConfig(config) {
		return nil, fmt.Errorf("invalid app-proxy config version or port")
	}
	return &config, nil
}

func validConfig(config Config) bool {
	return config.Version == configVersion && config.Port >= minPort && config.Port <= maxPort
}

type appKind string

const (
	kindChatGPT appKind = "chatgpt"
	kindQuota   appKind = "quota-float"
)

func (kind appKind) String() string { return string(kind) }

func wrapperName(kind appKind) string {
	switch kind {
	case kindChatGPT:
		return chatGPTWrapperName
	case kindQuota:
		return quotaWrapperName
	default:
		return ""
	}
}

func (kind appKind) valid() bool { return kind == kindChatGPT || kind == kindQuota }

type desktopTarget struct {
	kind    appKind
	command string
}

type filePlan struct {
	Path       string
	Role       string
	Original   []byte
	Existed    bool
	Mode       fs.FileMode
	Input      []byte
	SourcePath string
}

type journalFile struct {
	Path            string `json:"path"`
	Role            string `json:"role"`
	Existed         bool   `json:"existed"`
	Mode            uint32 `json:"mode"`
	OriginalBase64  string `json:"originalBase64,omitempty"`
	OriginalSHA256  string `json:"originalSha256,omitempty"`
	InstalledSHA256 string `json:"installedSha256"`
	Backup          string `json:"backup,omitempty"`
}

type journalSource struct {
	Path           string `json:"path"`
	OriginalBase64 string `json:"originalBase64"`
	OriginalSHA256 string `json:"originalSha256"`
	Backup         string `json:"backup"`
}

type journal struct {
	Version      int             `json:"version"`
	Port         int             `json:"port"`
	ConfigSHA256 string          `json:"configSha256"`
	Files        []journalFile   `json:"files"`
	Sources      []journalSource `json:"sources,omitempty"`
	CreatedAt    string          `json:"createdAt"`
	BackupSet    string          `json:"backupSet"`
}

type wrapperPlan struct {
	Kind     appKind
	Path     string
	Target   string
	Original []byte
	Existed  bool
	Mode     fs.FileMode
}

type installPlan struct {
	Home        string
	Port        int
	Files       []filePlan
	Sources     []journalSource
	Wrappers    []wrapperPlan
	WrapperPath map[appKind]string
}

// Run dispatches the app-proxy CLI.  The parent command can call this before
// parsing its network-controller flags.
func Run(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("无法确定当前用户 home 目录: %w", err)
	}
	return runAtHome(home, args, os.Stdout, os.Stderr)
}

func runAtHome(home string, args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}
	switch args[0] {
	case "--help", "-h", "help":
		printUsage(stdout)
		return nil
	case "install":
		if os.Geteuid() == 0 {
			return errors.New("app-proxy install 必须由桌面普通用户执行，不能使用 root")
		}
		port, err := parseInstallPort(args[1:], stderr)
		if err != nil {
			return err
		}
		if err := installAtHome(home, port, defaultApplicationDirs()); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "已安装 app-proxy，Clash HTTP 端口 %d\n", port)
		return nil
	case "status":
		config, err := ReadConfig(home)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stdout, "disabled")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "enabled port=%d\n", config.Port)
		return nil
	case "remove":
		if os.Geteuid() == 0 {
			return errors.New("app-proxy remove 必须由桌面普通用户执行，不能使用 root")
		}
		warnings, err := removeAtHome(home, defaultApplicationDirs())
		for _, warning := range warnings {
			fmt.Fprintf(stderr, "app-proxy: %s\n", warning)
		}
		return err
	case "run":
		if os.Geteuid() == 0 {
			return errors.New("app-proxy run 必须由桌面普通用户执行，不能使用 root")
		}
		if len(args) < 2 {
			return errors.New("用法: bknetwork app-proxy run chatgpt|quota-float [args...]")
		}
		kind := appKind(args[1])
		if !kind.valid() {
			return fmt.Errorf("不支持的 app-proxy 应用 %q", args[1])
		}
		return runWrappedAtHome(home, kind, args[2:])
	default:
		return fmt.Errorf("未知 app-proxy 命令 %q；使用 bknetwork app-proxy --help 查看帮助", args[0])
	}
}

func printUsage(out *os.File) {
	fmt.Fprintln(out, "用法: bknetwork app-proxy <install|status|remove|run>")
	fmt.Fprintln(out, "  install --port PORT     为 ChatGPT 与 quota-float 安装当前用户的 Clash HTTP wrapper")
	fmt.Fprintln(out, "  status                  查看当前用户的 app-proxy 标记")
	fmt.Fprintln(out, "  remove                  恢复安装前的菜单/自启动文件并移除 wrapper")
	fmt.Fprintln(out, "  run chatgpt [args...]   通过 wrapper 启动 ChatGPT")
	fmt.Fprintln(out, "  run quota-float [args...] 通过 wrapper 启动 quota-float")
	fmt.Fprintln(out, "说明: 只改用户本地 desktop override 与原本存在的自启动项，不修改全局环境或系统 desktop。")
	fmt.Fprintln(out, "说明: Quota Float 的 Tauri 开机启动开关会重写其 desktop 文件；启用后请保留本 wrapper，避免再次打开该开关覆盖 Exec。")
}

func parseInstallPort(args []string, stderr *os.File) (int, error) {
	flags := flag.NewFlagSet("app-proxy install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	port := flags.Int("port", 0, "Clash HTTP/mixed port")
	if err := flags.Parse(args); err != nil {
		return 0, err
	}
	if flags.NArg() != 0 {
		return 0, fmt.Errorf("install 不支持位置参数: %v", flags.Args())
	}
	if *port < minPort || *port > maxPort {
		return 0, fmt.Errorf("Clash 端口必须在 %d-%d 之间", minPort, maxPort)
	}
	return *port, nil
}

func defaultApplicationDirs() []string {
	return []string{"/usr/local/share/applications", "/usr/share/applications"}
}

func configDir(home string) string { return filepath.Dir(ConfigPath(home)) }

func journalPath(home string) string { return filepath.Join(configDir(home), journalName) }

func backupDir(home string) string { return filepath.Join(configDir(home), backupDirName) }

func wrapperDir(home string) string { return filepath.Join(configDir(home), wrapperDirName) }

func wrapperPath(home string, kind appKind) string {
	return filepath.Join(wrapperDir(home), wrapperName(kind))
}

func installAtHome(home string, port int, applicationDirs []string) error {
	if home == "" {
		return errors.New("home 目录不能为空")
	}
	if !validConfig(Config{Version: configVersion, Port: port}) {
		return fmt.Errorf("Clash 端口必须在 %d-%d 之间", minPort, maxPort)
	}
	if raw, err := readBoundedRegular(ConfigPath(home), 16*1024); err == nil {
		var existing Config
		if json.Unmarshal(raw, &existing) == nil && validConfig(existing) {
			if existing.Port == port {
				return errors.New("app-proxy 已安装；如需重新安装请先执行 remove")
			}
			return fmt.Errorf("app-proxy 已使用端口 %d；请先执行 remove 再安装新端口", existing.Port)
		}
		return errors.New("发现无效的 app-proxy config.json；为避免覆盖用户文件，请先检查或移除它")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取 app-proxy config.json 失败: %w", err)
	}
	if _, err := os.Stat(journalPath(home)); err == nil {
		return errors.New("发现未完成的 app-proxy journal；请先执行 remove 恢复")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取 app-proxy journal 失败: %w", err)
	}

	plan, err := buildInstallPlan(home, port, applicationDirs)
	if err != nil {
		return err
	}
	if len(plan.Files) == 0 {
		return errors.New("未找到现有的 ChatGPT 或 quota-float desktop 菜单/自启动项")
	}

	if err := os.MkdirAll(configDir(home), 0o700); err != nil {
		return fmt.Errorf("创建 app-proxy 配置目录失败: %w", err)
	}
	if err := os.MkdirAll(backupDir(home), 0o700); err != nil {
		return fmt.Errorf("创建 app-proxy 备份目录失败: %w", err)
	}
	if err := os.MkdirAll(wrapperDir(home), 0o700); err != nil {
		return fmt.Errorf("创建 app-proxy wrapper 目录失败: %w", err)
	}

	j := journalFromPlan(plan)
	journalBytes, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("生成 app-proxy journal 失败: %w", err)
	}
	// The journal is the first durable record.  It includes original bytes so
	// recovery remains possible even if the process dies while copying backups.
	if err := writeAtomic(journalPath(home), append(journalBytes, '\n'), 0o600); err != nil {
		return fmt.Errorf("保存 app-proxy journal 失败: %w", err)
	}

	if err := writeBackups(home, &j); err != nil {
		return rollbackInstall(home, j, fmt.Errorf("保存 app-proxy 备份失败: %w", err))
	}
	if err := writeWrappers(home, plan); err != nil {
		return rollbackInstall(home, j, fmt.Errorf("写入 app-proxy wrapper 失败: %w", err))
	}
	if err := writePlannedFiles(home, plan); err != nil {
		return rollbackInstall(home, j, fmt.Errorf("写入 desktop 文件失败: %w", err))
	}

	config := Config{Version: configVersion, Port: port}
	configBytes, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return rollbackInstall(home, j, fmt.Errorf("生成 app-proxy config 失败: %w", err))
	}
	configBytes = append(configBytes, '\n')
	j.ConfigSHA256 = sha256Hex(configBytes)
	// Keep the journal's config hash current before the marker is published.
	journalBytes, err = json.MarshalIndent(j, "", "  ")
	if err != nil {
		return rollbackInstall(home, j, fmt.Errorf("更新 app-proxy journal 失败: %w", err))
	}
	if err := writeAtomic(journalPath(home), append(journalBytes, '\n'), 0o600); err != nil {
		return rollbackInstall(home, j, fmt.Errorf("更新 app-proxy journal 失败: %w", err))
	}
	// The config marker is intentionally written last.  A missing marker makes
	// the install disabled while the journal still describes recovery.
	if err := writeAtomic(ConfigPath(home), configBytes, 0o600); err != nil {
		return rollbackInstall(home, j, fmt.Errorf("写入 app-proxy config 失败: %w", err))
	}
	return nil
}

func buildInstallPlan(home string, port int, applicationDirs []string) (installPlan, error) {
	plan := installPlan{
		Home:        home,
		Port:        port,
		WrapperPath: map[appKind]string{},
	}
	for _, kind := range []appKind{kindChatGPT, kindQuota} {
		plan.WrapperPath[kind] = wrapperPath(home, kind)
	}

	menuPlans, sources, err := discoverMenuPlans(home, applicationDirs)
	if err != nil {
		return plan, err
	}
	plan.Files = append(plan.Files, menuPlans...)
	plan.Sources = append(plan.Sources, sources...)

	autostartPlans, err := discoverAutostartPlans(home)
	if err != nil {
		return plan, err
	}
	plan.Files = append(plan.Files, autostartPlans...)

	seen := make(map[string]struct{}, len(plan.Files))
	deduped := plan.Files[:0]
	for _, item := range plan.Files {
		if _, ok := seen[item.Path]; ok {
			continue
		}
		seen[item.Path] = struct{}{}
		deduped = append(deduped, item)
	}
	plan.Files = deduped
	if len(plan.Files) > maxJournalEntries {
		return plan, fmt.Errorf("app-proxy 目标文件过多（最多 %d 个）", maxJournalEntries)
	}

	targets := map[appKind][]string{kindChatGPT: {}, kindQuota: {}}
	for _, item := range plan.Files {
		for _, target := range desktopTargets(item.Input) {
			targets[target.kind] = append(targets[target.kind], target.command)
		}
	}
	for _, kind := range []appKind{kindChatGPT, kindQuota} {
		target := chooseTarget(targets[kind], kind.String())
		wrapper := wrapperPlan{
			Kind:    kind,
			Path:    plan.WrapperPath[kind],
			Target:  target,
			Mode:    0o700,
			Existed: false,
		}
		if original, mode, wrapperErr := readRegular(wrapper.Path); wrapperErr == nil {
			wrapper.Original = original
			wrapper.Existed = true
			wrapper.Mode = mode
		} else if !errors.Is(wrapperErr, os.ErrNotExist) {
			return plan, fmt.Errorf("读取已有 app-proxy wrapper %s 失败: %w", wrapper.Path, wrapperErr)
		}
		plan.Wrappers = append(plan.Wrappers, wrapper)
	}
	return plan, nil
}

func discoverMenuPlans(home string, applicationDirs []string) ([]filePlan, []journalSource, error) {
	localDir := filepath.Join(home, filepath.FromSlash(localApplicationsRelativePath))
	byBase := make(map[string]string)
	for _, dir := range applicationDirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("读取 desktop 目录 %s 失败: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".desktop") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if _, ok := byBase[entry.Name()]; !ok {
				byBase[entry.Name()] = path
			}
		}
	}

	// A user-only override is still an existing menu and must be considered.
	if entries, err := os.ReadDir(localDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".desktop") {
				continue
			}
			path := filepath.Join(localDir, entry.Name())
			if _, ok := byBase[entry.Name()]; !ok {
				byBase[entry.Name()] = path
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("读取用户 desktop 目录失败: %w", err)
	}

	names := make([]string, 0, len(byBase))
	for name := range byBase {
		names = append(names, name)
	}
	sort.Strings(names)
	var plans []filePlan
	var sources []journalSource
	for _, name := range names {
		systemPath := byBase[name]
		if filepath.Dir(systemPath) == localDir {
			data, mode, err := readRegular(systemPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, nil, err
			}
			if len(data) > maxDesktopFileBytes || len(desktopTargets(data)) == 0 {
				continue
			}
			plans = append(plans, filePlan{Path: systemPath, Role: "menu", Original: data, Existed: true, Mode: mode, Input: data})
			continue
		}

		systemData, systemMode, err := readRegular(systemPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, nil, err
		}
		if len(systemData) > maxDesktopFileBytes || len(desktopTargets(systemData)) == 0 {
			continue
		}
		localPath := filepath.Join(localDir, name)
		if localData, localMode, localErr := readRegular(localPath); localErr == nil {
			if len(localData) > maxDesktopFileBytes || len(desktopTargets(localData)) == 0 {
				continue
			}
			plans = append(plans, filePlan{Path: localPath, Role: "menu", Original: localData, Existed: true, Mode: localMode, Input: localData, SourcePath: systemPath})
			continue
		} else if !errors.Is(localErr, os.ErrNotExist) {
			return nil, nil, localErr
		}
		plans = append(plans, filePlan{Path: localPath, Role: "menu", Original: nil, Existed: false, Mode: systemMode, Input: systemData, SourcePath: systemPath})
		sources = append(sources, makeSourceRecord(systemPath, systemData))
	}
	return plans, sources, nil
}

func discoverAutostartPlans(home string) ([]filePlan, error) {
	dir := filepath.Join(home, filepath.FromSlash(autostartRelativePath))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取用户自启动目录失败: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".desktop") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	var plans []filePlan
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, mode, err := readRegular(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if len(data) > maxDesktopFileBytes || len(desktopTargets(data)) == 0 {
			continue
		}
		plans = append(plans, filePlan{Path: path, Role: "autostart", Original: data, Existed: true, Mode: mode, Input: data})
	}
	return plans, nil
}

func makeSourceRecord(path string, data []byte) journalSource {
	return journalSource{
		Path:           path,
		OriginalBase64: base64.StdEncoding.EncodeToString(data),
		OriginalSHA256: sha256Hex(data),
	}
}

func chooseTarget(commands []string, fallback string) string {
	if len(commands) == 0 {
		return fallback
	}
	for _, command := range commands {
		if filepath.IsAbs(command) {
			return command
		}
	}
	return commands[0]
}

func journalFromPlan(plan installPlan) journal {
	j := journal{
		Version:   configVersion,
		Port:      plan.Port,
		Sources:   append([]journalSource(nil), plan.Sources...),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		BackupSet: "install-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	}
	for _, item := range plan.Files {
		installed := transformDesktop(item.Input, plan.WrapperPath)
		entry := journalFile{
			Path:            item.Path,
			Role:            item.Role,
			Existed:         item.Existed,
			Mode:            uint32(item.Mode.Perm()),
			OriginalSHA256:  sha256Hex(item.Original),
			InstalledSHA256: sha256Hex(installed),
		}
		if item.Existed {
			entry.OriginalBase64 = base64.StdEncoding.EncodeToString(item.Original)
		}
		j.Files = append(j.Files, entry)
	}
	for _, item := range plan.Wrappers {
		entry := journalFile{
			Path:            item.Path,
			Role:            "wrapper:" + item.Kind.String(),
			Existed:         item.Existed,
			Mode:            uint32(item.Mode.Perm()),
			InstalledSHA256: sha256Hex(wrapperContents(item.Kind, plan.Port, item.Target)),
		}
		if item.Existed {
			entry.OriginalBase64 = base64.StdEncoding.EncodeToString(item.Original)
			entry.OriginalSHA256 = sha256Hex(item.Original)
		}
		j.Files = append(j.Files, entry)
	}
	return j
}

func writeBackups(home string, j *journal) error {
	if j.BackupSet == "" || strings.ContainsAny(j.BackupSet, `/\\`) || j.BackupSet == "." || j.BackupSet == ".." {
		return errors.New("app-proxy backup set 无效")
	}
	setDir := filepath.Join(backupDir(home), j.BackupSet)
	if err := os.MkdirAll(setDir, 0o700); err != nil {
		return err
	}
	for index := range j.Sources {
		path := filepath.Join(setDir, fmt.Sprintf("source-%03d.desktop", index))
		data, err := base64.StdEncoding.DecodeString(j.Sources[index].OriginalBase64)
		if err != nil {
			return err
		}
		if err := writeAtomic(path, data, 0o600); err != nil {
			return err
		}
		j.Sources[index].Backup = filepath.Join(j.BackupSet, filepath.Base(path))
	}
	for index := range j.Files {
		if !j.Files[index].Existed || j.Files[index].OriginalBase64 == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(j.Files[index].OriginalBase64)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("file-%03d-%s.desktop", index, shortHash(j.Files[index].Path))
		if strings.HasPrefix(j.Files[index].Role, "wrapper:") {
			name = fmt.Sprintf("file-%03d-%s.wrapper", index, shortHash(j.Files[index].Path))
		}
		path := filepath.Join(setDir, name)
		if err := writeAtomic(path, data, 0o600); err != nil {
			return err
		}
		j.Files[index].Backup = filepath.Join(j.BackupSet, filepath.Base(path))
	}
	return nil
}

func writeWrappers(home string, plan installPlan) error {
	for _, item := range plan.Wrappers {
		data := wrapperContents(item.Kind, plan.Port, item.Target)
		mode := item.Mode
		if mode == 0 {
			mode = 0o700
		}
		if err := writeAtomic(item.Path, data, mode); err != nil {
			return err
		}
	}
	return nil
}

func writePlannedFiles(home string, plan installPlan) error {
	_ = home
	for _, item := range plan.Files {
		data := transformDesktop(item.Input, plan.WrapperPath)
		mode := item.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := writeAtomic(item.Path, data, mode); err != nil {
			return err
		}
	}
	return nil
}

func rollbackInstall(home string, j journal, cause error) error {
	warnings, restoreErr := restoreJournalFiles(home, j)
	for _, warning := range warnings {
		cause = errors.Join(cause, errors.New(warning))
	}
	if restoreErr != nil {
		cause = errors.Join(cause, restoreErr)
	}
	return cause
}

func removeAtHome(home string, applicationDirs []string) ([]string, error) {
	_ = applicationDirs
	path := journalPath(home)
	raw, err := readBoundedRegular(path, 8*1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		if _, configErr := os.Stat(ConfigPath(home)); configErr == nil {
			return nil, errors.New("app-proxy config 存在但 journal 缺失，拒绝盲目删除")
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 app-proxy journal 失败: %w", err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil || j.Version != configVersion || !validPort(j.Port) || len(j.Files) > maxJournalEntries {
		return nil, errors.New("app-proxy journal 无效，拒绝恢复")
	}
	if err := validateJournalPaths(home, j); err != nil {
		return nil, err
	}
	configPath := ConfigPath(home)
	configRaw, configErr := readBoundedRegular(configPath, 16*1024)
	if configErr == nil {
		if j.ConfigSHA256 == "" || sha256Hex(configRaw) != j.ConfigSHA256 {
			return nil, errors.New("app-proxy config 已被用户修改，保留整个安装并停止清理")
		}
	} else if !errors.Is(configErr, os.ErrNotExist) {
		return nil, fmt.Errorf("读取 app-proxy config 失败: %w", configErr)
	}
	warnings, err := restoreJournalFiles(home, j)
	if err != nil {
		return warnings, err
	}
	// Recheck after file restoration so a config changed during the operation
	// is never blindly deleted.  Normal callers are single-user operations;
	// this second check also handles an editor saving the marker concurrently.
	configRaw, configErr = readBoundedRegular(configPath, 16*1024)
	if configErr == nil {
		if j.ConfigSHA256 == "" || sha256Hex(configRaw) != j.ConfigSHA256 {
			return warnings, errors.New("app-proxy config 已被用户修改，保留该文件并停止清理")
		}
		if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return warnings, fmt.Errorf("移除 app-proxy config 失败: %w", err)
		}
	} else if !errors.Is(configErr, os.ErrNotExist) {
		return warnings, fmt.Errorf("读取 app-proxy config 失败: %w", configErr)
	}
	// Keep the journal and backups inspectable after a normal remove, but move
	// the active journal aside so a later install can start a fresh transaction.
	archive := filepath.Join(configDir(home), fmt.Sprintf("journal.removed-%d.json", time.Now().UnixNano()))
	if err := os.Rename(path, archive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return warnings, fmt.Errorf("归档 app-proxy journal 失败: %w", err)
	}
	return warnings, nil
}

func validateJournalPaths(home string, j journal) error {
	root, err := filepath.Abs(configDir(home))
	if err != nil {
		return err
	}
	for _, item := range j.Files {
		if err := ensureUnderHome(home, item.Path); err != nil {
			return fmt.Errorf("journal 路径不安全: %w", err)
		}
		if strings.HasPrefix(item.Role, "wrapper:") {
			path, err := filepath.Abs(item.Path)
			if err != nil || !pathWithin(root, path) {
				return errors.New("journal wrapper 路径不在 app-proxy 目录")
			}
		}
	}
	return nil
}

func restoreJournalFiles(home string, j journal) ([]string, error) {
	type restoreAction struct {
		item     journalFile
		original []byte
		remove   bool
	}

	var warnings []string
	var failures []error
	actions := make([]restoreAction, 0, len(j.Files))
	for _, item := range j.Files {
		current, currentErr := readRegularBytes(item.Path)
		if errors.Is(currentErr, os.ErrNotExist) {
			// A user can turn off an existing autostart entry after install;
			// preserve that deliberate deletion.  A missing menu or wrapper
			// needs attention because restoring it may be user-visible.
			if item.Existed && item.Role != "autostart" {
				failures = append(failures, fmt.Errorf("%s 原文件已不存在，无法自动恢复", item.Path))
			}
			continue
		}
		if currentErr != nil {
			failures = append(failures, fmt.Errorf("读取 %s 失败: %w", item.Path, currentErr))
			continue
		}

		currentHash := sha256Hex(current)
		var original []byte
		originalLoaded := false
		loadOriginal := func() error {
			if originalLoaded {
				return nil
			}
			if !item.Existed {
				originalLoaded = true
				return nil
			}
			if item.OriginalBase64 == "" {
				return errors.New("原始备份为空")
			}
			decoded, err := base64.StdEncoding.DecodeString(item.OriginalBase64)
			if err != nil {
				return err
			}
			if item.OriginalSHA256 != "" && sha256Hex(decoded) != item.OriginalSHA256 {
				return errors.New("原始备份校验失败")
			}
			original = decoded
			originalLoaded = true
			return nil
		}

		// A retry may encounter a file that was already restored before a
		// previous remove attempt failed.  Treat that state as completed,
		// rather than as a user conflict.
		if item.Existed {
			originalHash := item.OriginalSHA256
			if originalHash == "" {
				if err := loadOriginal(); err != nil {
					failures = append(failures, fmt.Errorf("解析 %s 的原始备份失败: %w", item.Path, err))
					continue
				}
				originalHash = sha256Hex(original)
			}
			if currentHash == originalHash {
				continue
			}
		}
		if item.InstalledSHA256 == "" {
			failures = append(failures, fmt.Errorf("%s 的安装内容校验值为空", item.Path))
			continue
		}
		if currentHash != item.InstalledSHA256 {
			warnings = append(warnings, fmt.Sprintf("检测到用户已修改 %s，保留当前内容", item.Path))
			failures = append(failures, fmt.Errorf("%s 存在用户修改，拒绝部分恢复", item.Path))
			continue
		}
		if item.Existed {
			if err := loadOriginal(); err != nil {
				failures = append(failures, fmt.Errorf("解析 %s 的原始备份失败: %w", item.Path, err))
				continue
			}
			actions = append(actions, restoreAction{item: item, original: original})
		} else {
			actions = append(actions, restoreAction{item: item, remove: true})
		}
	}
	if len(failures) > 0 {
		return warnings, errors.Join(failures...)
	}

	// Apply only after every target has passed the preflight above.  A user
	// edit therefore leaves the complete installation intact for a later
	// retry, instead of removing a wrapper while another desktop file still
	// points at it.
	for _, action := range actions {
		if action.remove {
			if err := os.Remove(action.item.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("移除生成的 %s 失败: %w", action.item.Path, err))
			}
			continue
		}
		mode := fs.FileMode(action.item.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := writeAtomic(action.item.Path, action.original, mode); err != nil {
			failures = append(failures, fmt.Errorf("恢复 %s 失败: %w", action.item.Path, err))
		}
	}
	if len(failures) > 0 {
		return warnings, errors.Join(failures...)
	}
	_ = home
	return warnings, nil
}

func runWrappedAtHome(home string, kind appKind, args []string) error {
	config, err := ReadConfig(home)
	if err != nil {
		return fmt.Errorf("app-proxy 未启用: %w", err)
	}
	_ = config
	path := wrapperPath(home, kind)
	if _, err := readRegularBytes(path); err != nil {
		return fmt.Errorf("app-proxy wrapper 不存在，请重新安装: %w", err)
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func wrapperContents(kind appKind, port int, target string) []byte {
	proxy := fmt.Sprintf("http://127.0.0.1:%d", port)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "proxy=%s\n", shellQuote(proxy))
	b.WriteString("export HTTP_PROXY=\"$proxy\"\n")
	b.WriteString("export HTTPS_PROXY=\"$proxy\"\n")
	b.WriteString("export ALL_PROXY=\"$proxy\"\n")
	b.WriteString("export http_proxy=\"$proxy\"\n")
	b.WriteString("export https_proxy=\"$proxy\"\n")
	b.WriteString("export all_proxy=\"$proxy\"\n")
	b.WriteString("export NO_PROXY='localhost,127.0.0.1,::1'\n")
	b.WriteString("export no_proxy=\"$NO_PROXY\"\n")
	switch kind {
	case kindChatGPT:
		fmt.Fprintf(&b, "exec %s --proxy-server=\"$proxy\" '--proxy-bypass-list=localhost;127.0.0.1;[::1]' \"$@\"\n", shellQuote(target))
	case kindQuota:
		fmt.Fprintf(&b, "exec %s \"$@\"\n", shellQuote(target))
	}
	return []byte(b.String())
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func desktopQuote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !desktopSafeRune(r)
	}) == -1 {
		return value
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\', '"', '`', '$':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func desktopSafeRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		strings.ContainsRune("_./:@+-", r)
}

func transformDesktop(data []byte, wrappers map[appKind]string) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		line := data
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			line = data[:index+1]
			data = data[index+1:]
		} else {
			data = nil
		}
		content := line
		newline := []byte{}
		if len(content) > 0 && content[len(content)-1] == '\n' {
			content = content[:len(content)-1]
			newline = []byte{'\n'}
			if len(content) > 0 && content[len(content)-1] == '\r' {
				content = content[:len(content)-1]
				newline = []byte{'\r', '\n'}
			}
		}
		leading := 0
		for leading < len(content) && (content[leading] == ' ' || content[leading] == '\t') {
			leading++
		}
		if leading >= len(content) || content[leading] == '#' || !bytes.HasPrefix(content[leading:], []byte("Exec=")) {
			out.Write(content)
			out.Write(newline)
			continue
		}
		valueStart := leading + len("Exec=")
		value := content[valueStart:]
		target, tokenStart, tokenEnd, ok := findDesktopTarget(value)
		if !ok {
			out.Write(content)
			out.Write(newline)
			continue
		}
		wrapper, exists := wrappers[target.kind]
		if !exists || wrapper == "" {
			out.Write(content)
			out.Write(newline)
			continue
		}
		out.Write(content[:valueStart])
		out.Write(value[:tokenStart])
		out.WriteString(desktopQuote(wrapper))
		out.Write(value[tokenEnd:])
		out.Write(newline)
	}
	return out.Bytes()
}

func desktopTargets(data []byte) []desktopTarget {
	var result []desktopTarget
	for len(data) > 0 {
		line := data
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			line = data[:index]
			data = data[index+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		leading := 0
		for leading < len(line) && (line[leading] == ' ' || line[leading] == '\t') {
			leading++
		}
		if leading >= len(line) || line[leading] == '#' || !bytes.HasPrefix(line[leading:], []byte("Exec=")) {
			continue
		}
		if target, _, _, ok := findDesktopTarget(line[leading+len("Exec="):]); ok && !isGeneratedWrapper(target.command) {
			result = append(result, target)
		}
	}
	return result
}

func findDesktopTarget(value []byte) (desktopTarget, int, int, bool) {
	start, end, token, ok := nextDesktopToken(value, 0)
	if !ok {
		return desktopTarget{}, 0, 0, false
	}
	if kind := kindForCommand(token); kind.valid() {
		return desktopTarget{kind: kind, command: token}, start, end, true
	}
	// Tolerate the common `env VAR=value chatgpt ...` form while keeping the
	// direct desktop Exec form byte-for-byte apart from its command token.
	if filepath.Base(token) != "env" {
		return desktopTarget{}, 0, 0, false
	}
	position := end
	for {
		start, end, token, ok = nextDesktopToken(value, position)
		if !ok {
			return desktopTarget{}, 0, 0, false
		}
		position = end
		if strings.Contains(token, "=") && !strings.HasPrefix(token, "/") {
			continue
		}
		if kind := kindForCommand(token); kind.valid() {
			return desktopTarget{kind: kind, command: token}, start, end, true
		}
		return desktopTarget{}, 0, 0, false
	}
}

func kindForCommand(command string) appKind {
	base := strings.ToLower(filepath.Base(command))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "chatgpt", "chatgpt-clash.sh":
		return kindChatGPT
	case "quota-float", "quota-float-clash.sh":
		return kindQuota
	default:
		return ""
	}
}

func isGeneratedWrapper(command string) bool {
	base := strings.ToLower(filepath.Base(command))
	return base == chatGPTWrapperName || base == quotaWrapperName
}

func nextDesktopToken(value []byte, offset int) (start, end int, token string, ok bool) {
	for offset < len(value) && (value[offset] == ' ' || value[offset] == '\t') {
		offset++
	}
	if offset >= len(value) {
		return 0, 0, "", false
	}
	start = offset
	var b strings.Builder
	var quote byte
	for offset < len(value) {
		c := value[offset]
		if quote == 0 && (c == ' ' || c == '\t') {
			break
		}
		if c == '\\' && offset+1 < len(value) {
			offset++
			b.WriteByte(value[offset])
			offset++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
				offset++
				continue
			}
			b.WriteByte(c)
			offset++
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			offset++
			continue
		}
		b.WriteByte(c)
		offset++
	}
	return start, offset, b.String(), b.Len() > 0
}

func validPort(port int) bool { return port >= minPort && port <= maxPort }

func readRegular(path string) ([]byte, fs.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("目标不是普通文件: %s", path)
	}
	data, err := os.ReadFile(path)
	return data, info.Mode(), err
}

func readRegularBytes(path string) ([]byte, error) {
	data, _, err := readRegular(path)
	return data, err
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".appproxy-*"+filepath.Ext(path))
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func sha256Hex(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func shortHash(path string) string {
	value := sha256Hex([]byte(path))
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func ensureUnderHome(home, path string) error {
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !pathWithin(homeAbs, pathAbs) {
		return fmt.Errorf("%s 不在 home 目录 %s 内", path, home)
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func readBoundedRegular(path string, maxBytes int64) ([]byte, error) {
	// ReadConfig is called by the root service on a path controlled by the
	// desktop user.  Open with O_NOFOLLOW/O_NONBLOCK and verify the descriptor
	// itself so a symlink or FIFO cannot make the service follow another file or
	// block while reading the marker.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("无法打开 app-proxy config")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("app-proxy config 不是普通文件")
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("app-proxy config is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("app-proxy config is too large")
	}
	return data, nil
}
