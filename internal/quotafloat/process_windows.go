//go:build windows

package quotafloat

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	quotaFloatExecutable = "quota-float.exe"

	// The old instance is terminated only after a suspended replacement has
	// been created and verified. This bound prevents a stuck GUI from keeping
	// the newly-created instance suspended indefinitely.
	oldProcessWaitMilliseconds   = 10_000
	childProcessWaitMilliseconds = 5_000
)

// listProcesses deliberately starts with a Toolhelp snapshot and then
// re-checks every candidate through its process handle. A process name in a
// snapshot is not an identity: the PID can be reused while the snapshot is
// being walked, and an elevated BKNetwork process must not manage another
// session or account.
func listProcesses() ([]Process, error) {
	sessionID, err := currentSessionID()
	if err != nil {
		return nil, fmt.Errorf("get current Windows session: %w", err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("get current Windows user: %w", err)
	}

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("snapshot Windows processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, fmt.Errorf("enumerate Windows processes: %w", err)
	}

	var result []Process
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), quotaFloatExecutable) {
			if process, ok := inspectCandidate(entry.ProcessID, sessionID, currentSID); ok {
				result = append(result, process)
			}
		}

		err = windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("enumerate Windows processes: %w", err)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Started == result[j].Started {
			return result[i].PID < result[j].PID
		}
		return result[i].Started < result[j].Started
	})
	return result, nil
}

// inspectCandidate treats a process that exits or becomes inaccessible while
// listing it as a race and skips it. This keeps one unrelated process from
// preventing the adapter from finding another matching instance.
func inspectCandidate(pid, expectedSession uint32, expectedSID *windows.SID) (Process, bool) {
	if pid == 0 || pid == windows.GetCurrentProcessId() {
		return Process{}, false
	}
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION)
	processHandle, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return Process{}, false
	}
	defer windows.CloseHandle(processHandle)

	process, sid, session, err := inspectProcessHandle(processHandle)
	if err != nil || session != expectedSession || !sid.Equals(expectedSID) {
		return Process{}, false
	}
	if !isQuotaFloatPath(process.Path) {
		return Process{}, false
	}
	return process, true
}

// restartProcess performs a create-verify-switch transaction. In particular,
// it never terminates the old process before a suspended replacement has
// inherited the original process security context and passed validation.
func restartProcess(process Process, proxyAddress string) (Process, error) {
	if process.PID == 0 || process.Started == 0 || process.Path == "" {
		return Process{}, errors.New("quota-float process identity is incomplete")
	}
	if !filepath.IsAbs(process.Path) || !isQuotaFloatPath(process.Path) {
		return Process{}, errors.New("quota-float process path is not an absolute quota-float.exe path")
	}

	currentSession, err := currentSessionID()
	if err != nil {
		return Process{}, fmt.Errorf("get current Windows session: %w", err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		return Process{}, fmt.Errorf("get current Windows user: %w", err)
	}

	oldAccess := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_CREATE_PROCESS | windows.PROCESS_TERMINATE | windows.SYNCHRONIZE)
	oldHandle, err := windows.OpenProcess(oldAccess, false, process.PID)
	if err != nil {
		return Process{}, fmt.Errorf("open quota-float process %d: %w", process.PID, err)
	}
	defer windows.CloseHandle(oldHandle)

	actual, oldSID, oldSession, err := inspectProcessHandle(oldHandle)
	if err != nil {
		return Process{}, fmt.Errorf("inspect quota-float process %d: %w", process.PID, err)
	}
	if actual.Started != process.Started {
		return Process{}, fmt.Errorf("quota-float process %d changed before restart", process.PID)
	}
	if !samePath(actual.Path, process.Path) || !isQuotaFloatPath(actual.Path) {
		return Process{}, fmt.Errorf("quota-float process %d path changed before restart", process.PID)
	}
	if oldSession != currentSession || !oldSID.Equals(currentSID) {
		return Process{}, fmt.Errorf("quota-float process %d is outside the current user session", process.PID)
	}

	var token windows.Token
	const tokenAccess = windows.TOKEN_QUERY | windows.TOKEN_DUPLICATE
	if err := windows.OpenProcessToken(oldHandle, tokenAccess, &token); err != nil {
		return Process{}, fmt.Errorf("open quota-float process token: %w", err)
	}
	defer token.Close()

	var sourceEnvironment *uint16
	if err := windows.CreateEnvironmentBlock(&sourceEnvironment, token, false); err != nil {
		return Process{}, fmt.Errorf("create quota-float user environment: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(sourceEnvironment)

	baseEnvironment, err := environmentBlockToSlice(sourceEnvironment)
	if err != nil {
		return Process{}, fmt.Errorf("read quota-float user environment: %w", err)
	}
	childEnvironment, err := makeEnvironmentBlock(proxyEnvironment(baseEnvironment, proxyAddress))
	if err != nil {
		return Process{}, fmt.Errorf("prepare quota-float user environment: %w", err)
	}

	executable, err := windows.UTF16FromString(actual.Path)
	if err != nil {
		return Process{}, fmt.Errorf("encode quota-float path: %w", err)
	}
	workingDirectory, err := windows.UTF16FromString(filepath.Dir(actual.Path))
	if err != nil {
		return Process{}, fmt.Errorf("encode quota-float working directory: %w", err)
	}
	var processInformation windows.ProcessInformation

	// The source application does not have business startup arguments. Passing
	// lpApplicationName with a nil command line avoids inventing or incorrectly
	// quoting arguments. No CREATE_NO_WINDOW or hidden-window flag is used:
	// Quota Float is a GUI application and keeps its normal window behavior.
	if waitResult, err := windows.WaitForSingleObject(oldHandle, 0); err != nil {
		return Process{}, fmt.Errorf("check original quota-float before replacement: %w", err)
	} else if waitResult != uint32(windows.WAIT_TIMEOUT) {
		return Process{}, errors.New("original quota-float exited before replacement")
	}
	creationFlags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := createAsOriginalProcess(oldHandle, &executable[0], &childEnvironment[0], &workingDirectory[0], creationFlags, &processInformation); err != nil {
		return Process{}, fmt.Errorf("create suspended quota-float replacement: %w", err)
	}

	childHandle := processInformation.Process
	childThread := processInformation.Thread
	childOwned := true
	defer func() {
		if childOwned {
			terminateAndCloseChild(childHandle, childThread)
		}
	}()

	// Verify the new process before touching the old one. This also checks the
	// replacement token and session, rather than trusting CreateProcess' PID.
	childProcess, childSID, childSession, err := inspectProcessHandle(childHandle)
	if err != nil {
		return Process{}, fmt.Errorf("inspect suspended quota-float replacement: %w", err)
	}
	if childProcess.PID != processInformation.ProcessId || childSession != currentSession || !childSID.Equals(currentSID) || !samePath(childProcess.Path, actual.Path) || !isQuotaFloatPath(childProcess.Path) {
		return Process{}, errors.New("suspended quota-float replacement failed identity checks")
	}
	if waitResult, err := windows.WaitForSingleObject(childHandle, 0); err != nil {
		return Process{}, fmt.Errorf("check suspended quota-float replacement: %w", err)
	} else if waitResult != uint32(windows.WAIT_TIMEOUT) {
		return Process{}, errors.New("suspended quota-float replacement exited before the switch")
	}

	if err := windows.TerminateProcess(oldHandle, 0); err != nil {
		// The old instance may have exited naturally between inspection and
		// TerminateProcess. It is safe to continue only when the wait confirms
		// that the original handle is signaled.
		if waitResult, waitErr := windows.WaitForSingleObject(oldHandle, 0); waitErr != nil || waitResult != windows.WAIT_OBJECT_0 {
			if waitErr != nil {
				return Process{}, fmt.Errorf("terminate old quota-float process %d: %w", process.PID, err)
			}
			return Process{}, fmt.Errorf("terminate old quota-float process %d: %w", process.PID, err)
		}
	}
	waitResult, err := windows.WaitForSingleObject(oldHandle, oldProcessWaitMilliseconds)
	if err != nil {
		return Process{}, fmt.Errorf("wait for old quota-float process %d: %w", process.PID, err)
	}
	if waitResult != windows.WAIT_OBJECT_0 {
		return Process{}, fmt.Errorf("old quota-float process %d did not exit in time", process.PID)
	}

	if _, err := windows.ResumeThread(childThread); err != nil {
		return Process{}, fmt.Errorf("resume quota-float replacement: %w", err)
	}
	childOwned = false
	windows.CloseHandle(childThread)
	childThread = windows.InvalidHandle
	windows.CloseHandle(childHandle)
	childHandle = windows.InvalidHandle
	return childProcess, nil
}

// createAsOriginalProcess uses the still-open original process as the Windows
// parent. Windows then inherits its token (including UAC and restrictions),
// instead of the potentially elevated BKNetwork token. This does not require
// SeAssignPrimaryTokenPrivilege, SeIncreaseQuotaPrivilege or a profile reload.
// The explicit environment comes from that user's standard environment;
// bInheritHandles=false prevents leaking handles from either process.
// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute
func createAsOriginalProcess(parent windows.Handle, executable, environment, workingDirectory *uint16, creationFlags uint32, processInformation *windows.ProcessInformation) error {
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("allocate original process startup attributes: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PARENT_PROCESS, unsafe.Pointer(&parent), unsafe.Sizeof(parent)); err != nil {
		return fmt.Errorf("set original quota-float parent: %w", err)
	}
	startupInfo := windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attributes.List(),
	}
	if err := windows.CreateProcess(executable, nil, nil, nil, false, creationFlags|windows.EXTENDED_STARTUPINFO_PRESENT, environment, workingDirectory, &startupInfo.StartupInfo, processInformation); err != nil {
		if processInformation.Process != 0 || processInformation.Thread != 0 {
			terminateAndCloseChild(processInformation.Process, processInformation.Thread)
			*processInformation = windows.ProcessInformation{}
		}
		return fmt.Errorf("CreateProcess with original quota-float parent: %w", err)
	}
	return nil
}

func inspectProcessHandle(handle windows.Handle) (Process, *windows.SID, uint32, error) {
	path, err := queryProcessPath(handle)
	if err != nil {
		return Process{}, nil, 0, fmt.Errorf("query executable path: %w", err)
	}
	started, err := queryProcessStartTime(handle)
	if err != nil {
		return Process{}, nil, 0, fmt.Errorf("query process start time: %w", err)
	}
	session, err := processSessionID(handle)
	if err != nil {
		return Process{}, nil, 0, fmt.Errorf("query process session: %w", err)
	}
	sid, err := processUserSID(handle)
	if err != nil {
		return Process{}, nil, 0, fmt.Errorf("query process user: %w", err)
	}
	pid, err := windows.GetProcessId(handle)
	if err != nil {
		return Process{}, nil, 0, fmt.Errorf("query process id: %w", err)
	}
	return Process{PID: pid, Started: started, Path: path}, sid, session, nil
}

func currentSessionID() (uint32, error) {
	return processSessionByPID(windows.GetCurrentProcessId())
}

func processSessionID(handle windows.Handle) (uint32, error) {
	pid, err := windows.GetProcessId(handle)
	if err != nil {
		return 0, err
	}
	return processSessionByPID(pid)
}

func processSessionByPID(pid uint32) (uint32, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(pid, &session); err != nil {
		return 0, err
	}
	return session, nil
}

func currentUserSID() (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	return tokenSID(token)
}

func processUserSID(handle windows.Handle) (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	return tokenSID(token)
}

func tokenSID(token windows.Token) (*windows.SID, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid == nil {
		return nil, errors.New("Windows token has no user SID")
	}
	return user.User.Sid, nil
}

func queryProcessStartTime(handle windows.Handle) (uint64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}

func queryProcessPath(handle windows.Handle) (string, error) {
	// QueryFullProcessImageNameW reports the required length through the
	// insufficient-buffer error. Keep the allocation bounded at MAX_LONG_PATH.
	for size := uint32(windows.MAX_PATH); ; {
		buffer := make([]uint16, size)
		length := uint32(len(buffer))
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size == windows.MAX_LONG_PATH {
			return "", err
		}
		next := size * 2
		if next > windows.MAX_LONG_PATH {
			next = windows.MAX_LONG_PATH
		}
		size = next
	}
}

func isQuotaFloatPath(path string) bool {
	return filepath.IsAbs(path) && strings.EqualFold(filepath.Base(path), quotaFloatExecutable)
}

func samePath(left, right string) bool {
	return normalizeProcessPath(left) == normalizeProcessPath(right)
}

func normalizeProcessPath(path string) string {
	path = filepath.Clean(path)
	if strings.HasPrefix(path, `\\?\`) {
		path = strings.TrimPrefix(path, `\\?\`)
	}
	return strings.ToUpper(path)
}

// environmentBlockToSlice reads the double-NUL-terminated block returned by
// CreateEnvironmentBlock. It never logs or returns the values outside this
// package; the caller only uses them to construct the child environment.
func environmentBlockToSlice(block *uint16) ([]string, error) {
	if block == nil {
		return nil, errors.New("Windows environment block is nil")
	}
	const maxEnvironmentCharacters = 1 << 20
	size := unsafe.Sizeof(*block)
	var result []string
	characters := 0
	for *block != 0 {
		start := block
		end := unsafe.Pointer(block)
		for *(*uint16)(end) != 0 {
			characters++
			if characters > maxEnvironmentCharacters {
				return nil, errors.New("Windows environment block is unexpectedly large")
			}
			end = unsafe.Add(end, size)
		}
		entry := unsafe.Slice(start, (uintptr(end)-uintptr(unsafe.Pointer(start)))/size)
		result = append(result, windows.UTF16ToString(entry))
		block = (*uint16)(unsafe.Add(end, size))
	}
	return result, nil
}

func makeEnvironmentBlock(environment []string) ([]uint16, error) {
	result := make([]uint16, 0, len(environment)*32+1)
	// CreateProcessAsUser requires a Unicode environment block sorted by
	// variable name, case-insensitively. proxyEnvironment intentionally keeps
	// its append order for the manager API, so sort a private copy here.
	sorted := append([]string(nil), environment...)
	sort.SliceStable(sorted, func(i, j int) bool {
		left, right := strings.ToUpper(environmentName(sorted[i])), strings.ToUpper(environmentName(sorted[j]))
		if left == right {
			return sorted[i] < sorted[j]
		}
		return left < right
	})
	for _, entry := range sorted {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("Windows environment entry contains NUL")
		}
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, encoded[:len(encoded)-1]...)
		result = append(result, 0)
	}
	// The block has one NUL after the last entry and one extra NUL marking the
	// end of the block. An empty block therefore consists of two NULs too.
	result = append(result, 0)
	if len(sorted) == 0 {
		result = append(result, 0)
	}
	return result, nil
}

func environmentName(entry string) string {
	if strings.HasPrefix(entry, "=") {
		if end := strings.IndexByte(entry[1:], '='); end >= 0 {
			return entry[:end+2]
		}
	}
	name, _, _ := strings.Cut(entry, "=")
	return name
}

func terminateAndCloseChild(processHandle, threadHandle windows.Handle) {
	if processHandle != 0 && processHandle != windows.InvalidHandle {
		_ = windows.TerminateProcess(processHandle, 1)
		_, _ = windows.WaitForSingleObject(processHandle, childProcessWaitMilliseconds)
	}
	if threadHandle != 0 && threadHandle != windows.InvalidHandle {
		_ = windows.CloseHandle(threadHandle)
	}
	if processHandle != 0 && processHandle != windows.InvalidHandle {
		_ = windows.CloseHandle(processHandle)
	}
}
