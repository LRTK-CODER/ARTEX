package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/klauspost/compress/zstd"
)

const (
	archiveDirMode  = 0o700
	archiveFileMode = 0o600
	maxArchiveFiles = 1_000_000
	maxArchiveBytes = int64(1 << 47) // 128 TiB safety ceiling for corrupt headers.
)

type archiveFileMove struct {
	Source   string `json:"source"`
	Relative string `json:"relative"`
}

type archiveStageJournal struct {
	ArchiveID int64             `json:"archive_id"`
	TaskID    string            `json:"task_id"`
	Moves     []archiveFileMove `json:"moves"`
}

type archiveRestoreJournal struct {
	ArchiveID int64                 `json:"archive_id"`
	TaskID    int64                 `json:"task_id"`
	Moves     []restoredArchivePath `json:"moves"`
}

type taskArchiveFileStage struct {
	root    string
	payload string
	journal archiveStageJournal
	done    bool
	// removeAll 는 commit이 임시 디렉터리를 지우는 함수다. nil이면 os.RemoveAll이다. uid 분리를 켜면 에이전트가
	// 권한을 바꾼 디렉터리도 지우는 AgentSandbox.RemoveAll을 넣는다(#179).
	removeAll func(string) error
}

type restoredArchivePath struct {
	Source      string
	Destination string
}

type taskArchiveRestoreFiles struct {
	extracted string
	moves     []restoredArchivePath
	done      bool
}

func writeArchiveJournal(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func taskArchiveRoot(dataDir string) string {
	return filepath.Join(dataDir, "archives", "tasks")
}

func taskArchivePath(dataDir string, archiveID int64, taskID string) string {
	return filepath.Join(taskArchiveRoot(dataDir), fmt.Sprintf("task-%s-%d.tar.zst", taskID, archiveID))
}

func stageTaskArchiveFiles(dataDir string, archiveID int64, taskID string, explorationID int64) (*taskArchiveFileStage, error) {
	root := filepath.Join(taskArchiveRoot(dataDir), ".staging", strconv.FormatInt(archiveID, 10))
	stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload")}
	journalPath := filepath.Join(root, "journal.json")
	if raw, err := os.ReadFile(journalPath); err == nil {
		if err := json.Unmarshal(raw, &stage.journal); err != nil {
			return nil, fmt.Errorf("read task archive staging journal: %w", err)
		}
		// The journal is written before the first rename, so its presence does not mean
		// the payload is complete: a previous round may have failed mid-loop with a
		// rollback that itself errored, which leaves the root in place. Replay the moves
		// rather than packaging a payload that is missing transcripts or workspace files.
		if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
			return nil, err
		}
		if err := stage.applyMoves(); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		return stage, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
		return nil, err
	}
	stage.journal = archiveStageJournal{ArchiveID: archiveID, TaskID: taskID}
	targets := []archiveFileMove{}
	taskDir := filepath.Join(dataDir, "tasks", taskID)
	if _, err := os.Lstat(taskDir); err == nil {
		targets = append(targets, archiveFileMove{Source: taskDir, Relative: filepath.Join("files", "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcriptDir := filepath.Join(dataDir, "transcripts")
	entries, err := os.ReadDir(transcriptDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	prefix := fmt.Sprintf("exp%d-", explorationID)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || (!entry.IsDir() && !strings.HasSuffix(name, ".jsonl")) {
			continue
		}
		targets = append(targets, archiveFileMove{
			Source: filepath.Join(transcriptDir, name), Relative: filepath.Join("files", "transcripts", name),
		})
	}
	// Persist the complete plan before the first rename. Recovery can therefore
	// roll back any prefix of the moves after an abrupt process termination.
	stage.journal.Moves = targets
	if err := writeArchiveJournal(journalPath, stage.journal); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	if err := stage.applyMoves(); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	return stage, nil
}

// applyMoves performs the renames recorded in the journal. Entries already sitting
// in the payload are skipped, so an interrupted staging round can be resumed in
// place without moving anything twice.
func (s *taskArchiveFileStage) applyMoves() error {
	for _, move := range s.journal.Moves {
		destination := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Lstat(move.Source); err != nil {
			if os.IsNotExist(err) {
				// Neither side exists: the path was removed outside the archive flow
				// after the journal was written. Nothing can be staged for it.
				continue
			}
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), archiveDirMode); err != nil {
			return err
		}
		if err := os.Rename(move.Source, destination); err != nil {
			return fmt.Errorf("stage task archive path %s: %w", move.Source, err)
		}
	}
	return nil
}

func (s *taskArchiveFileStage) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.journal.Moves) - 1; i >= 0; i-- {
		move := s.journal.Moves[i]
		staged := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(staged); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("archive rollback destination exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := os.RemoveAll(s.root); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = true
	return errors.Join(errs...)
}

func (s *taskArchiveFileStage) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	if s.removeAll != nil {
		return s.removeAll(s.root)
	}
	return os.RemoveAll(s.root)
}

func installTaskArchiveFiles(dataDir, extractedDir, taskID string, archiveID int64) (*taskArchiveRestoreFiles, error) {
	stage := &taskArchiveRestoreFiles{extracted: extractedDir}
	numericTaskID, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || numericTaskID <= 0 {
		return nil, fmt.Errorf("invalid restore task id %q", taskID)
	}
	sources := []restoredArchivePath{}
	workspace := filepath.Join(extractedDir, "files", "tasks", taskID)
	if _, err := os.Lstat(workspace); err == nil {
		sources = append(sources, restoredArchivePath{Source: workspace, Destination: filepath.Join(dataDir, "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcripts := filepath.Join(extractedDir, "files", "transcripts")
	entries, err := os.ReadDir(transcripts)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		sources = append(sources, restoredArchivePath{
			Source: filepath.Join(transcripts, entry.Name()), Destination: filepath.Join(dataDir, "transcripts", entry.Name()),
		})
	}
	for _, move := range sources {
		if _, err := os.Lstat(move.Destination); err == nil {
			return nil, fmt.Errorf("restore destination already exists: %s", move.Destination)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	stage.moves = sources
	journal := archiveRestoreJournal{ArchiveID: archiveID, TaskID: numericTaskID, Moves: sources}
	if err := writeArchiveJournal(filepath.Join(extractedDir, "restore-journal.json"), journal); err != nil {
		return nil, err
	}
	for _, move := range sources {
		if err := os.MkdirAll(filepath.Dir(move.Destination), 0o755); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		if err := os.Rename(move.Source, move.Destination); err != nil {
			_ = stage.rollback()
			return nil, err
		}
	}
	return stage, nil
}

func (s *taskArchiveRestoreFiles) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.moves) - 1; i >= 0; i-- {
		move := s.moves[i]
		if _, err := os.Lstat(move.Destination); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("restore rollback source exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), archiveDirMode); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(move.Destination, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = true
	return errors.Join(errs...)
}

func (s *taskArchiveRestoreFiles) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.extracted)
}

// recoverTaskArchiveRestoreStages resolves file installs left by an interrupted
// restore. If PostgreSQL committed, installed files are authoritative and only
// the extraction directory is stale. Otherwise all completed renames are moved
// back so the persistent restore job can retry from a clean destination.
func recoverTaskArchiveRestoreStages(dataDir string, pg *pgdb.DB) error {
	parent := filepath.Join(taskArchiveRoot(dataDir), ".restore")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "restore-journal.json"))
		if os.IsNotExist(err) {
			// Extraction was interrupted before any destination rename.
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveRestoreJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		restored, err := pg.IsTaskArchiveRestored(journal.ArchiveID)
		if errors.Is(err, pgdb.ErrTaskArchiveNotFound) && journal.TaskID > 0 {
			task, taskErr := pg.GetTask(journal.TaskID)
			if taskErr != nil {
				errs = append(errs, taskErr)
				continue
			}
			restored = task != nil
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if restored {
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveRestoreFiles{extracted: root, moves: journal.Moves}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverTaskArchiveStages resolves file renames left by an interrupted archive.
// A committed cold task already has a verified package, so stale staging can be
// discarded; otherwise files are moved back before the persistent job retries.
// removeAll 은 버릴 임시 디렉터리를 지운다(taskArchiveFileStage.removeAll과 같다). 에이전트가 권한을 바꾼 디렉터리가
// 남아 지우지 못하면 시작 복구가 실패해 보관 작업자가 멈추므로, uid 분리를 켜면 AgentSandbox.RemoveAll을 넣는다(#179).
func recoverTaskArchiveStages(dataDir string, pg *pgdb.DB, removeAll func(string) error) error {
	parent := filepath.Join(taskArchiveRoot(dataDir), ".staging")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "journal.json"))
		if os.IsNotExist(err) {
			// Either staging died between creating the directory tree and writing the
			// journal — no rename had run, so payload holds nothing — or commit/rollback
			// failed part-way through removing the root, in which case payload only holds
			// copies already inside the package. Without a journal there is nothing to
			// roll back, and reporting an error here keeps the archive worker from ever
			// starting, so discard the directory instead.
			if err := removeAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveStageJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		archive, getErr := pg.GetTaskArchive(journal.ArchiveID)
		if getErr != nil {
			errs = append(errs, getErr)
			continue
		}
		if archive != nil && archive.State == pgdb.ArchiveReady {
			if err := removeAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload"), journal: journal}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
		}
		if archive != nil && archive.ArchivePath != "" {
			_ = os.Remove(archive.ArchivePath)
		}
	}
	return errors.Join(errs...)
}

func recoverTaskArchiveDeletePackages(dataDir string, pg *pgdb.DB) error {
	root := taskArchiveRoot(dataDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		marker := strings.LastIndex(name, ".deleting-")
		if marker < 0 {
			continue
		}
		archiveID, err := strconv.ParseInt(name[marker+len(".deleting-"):], 10, 64)
		if err != nil || archiveID <= 0 {
			continue
		}
		staged := filepath.Join(root, name)
		original := filepath.Join(root, name[:marker])
		archive, err := pg.GetTaskArchive(archiveID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if archive == nil {
			if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if archive.State == pgdb.DeleteQueued || archive.State == pgdb.Deleting || archive.State == pgdb.DeleteFailed {
			// The idempotent delete worker consumes the staged path directly.
			continue
		}
		if _, err := os.Lstat(original); err == nil {
			errs = append(errs, fmt.Errorf("archive delete recovery destination exists: %s", original))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, original); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func stageTaskArchivePackageDelete(archivePath string, archiveID int64) (string, bool, error) {
	staged := archivePath + fmt.Sprintf(".deleting-%d", archiveID)
	if _, err := os.Lstat(staged); err == nil {
		if _, originalErr := os.Lstat(archivePath); originalErr == nil {
			return staged, false, errors.New("보관 패키지 원본 파일과 삭제용 임시 파일이 함께 있습니다")
		} else if !os.IsNotExist(originalErr) {
			return staged, false, originalErr
		}
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	if err := os.Rename(archivePath, staged); err == nil {
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	return staged, false, nil
}

// taskArchivePackage 는 writeTaskArchivePackage가 쓴 패키지의 크기·체크섬과, 서버가 읽지 못해 넣지 못한 항목이다.
type taskArchivePackage struct {
	originalSize   int64
	compressedSize int64
	checksum       string
	// skipped 는 payload 기준 경로(슬래시 구분)다. 에이전트가 권한을 바꿔(chmod 600·000, setfacl) 서버가 읽지 못한 것이다.
	skipped []string
}

func writeTaskArchivePackage(path, payloadDir string, snapshot *pgdb.TaskArchiveSnapshot) (taskArchivePackage, error) {
	var pkg taskArchivePackage
	if snapshot == nil {
		return pkg, errors.New("nil task archive snapshot")
	}
	if err := os.MkdirAll(filepath.Dir(path), archiveDirMode); err != nil {
		return pkg, err
	}
	manifestFile, err := os.OpenFile(
		filepath.Join(payloadDir, "manifest.json"),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		archiveFileMode,
	)
	if err != nil {
		return pkg, err
	}
	encodeErr := json.NewEncoder(manifestFile).Encode(snapshot)
	var syncErr error
	if encodeErr == nil {
		syncErr = manifestFile.Sync()
	}
	if err := errors.Join(encodeErr, syncErr, manifestFile.Close()); err != nil {
		return pkg, err
	}
	temporary := path + ".partial"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return pkg, err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	multi := io.MultiWriter(file, hasher)
	encoder, err := zstd.NewWriter(multi,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(10)),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return pkg, err
	}
	tarWriter := tar.NewWriter(encoder)
	skipped, walkErr := writeArchivePayload(tarWriter, payloadDir, &pkg.originalSize)
	if walkErr != nil {
		_ = tarWriter.Close()
		_ = encoder.Close()
		return pkg, walkErr
	}
	if err := tarWriter.Close(); err != nil {
		_ = encoder.Close()
		return pkg, err
	}
	if err := encoder.Close(); err != nil {
		return pkg, err
	}
	if err := file.Sync(); err != nil {
		return pkg, err
	}
	if err := file.Close(); err != nil {
		return pkg, err
	}
	stat, err := os.Stat(temporary)
	if err != nil {
		return pkg, err
	}
	pkg.compressedSize = stat.Size()
	pkg.checksum = hex.EncodeToString(hasher.Sum(nil))
	pkg.skipped = skipped
	if err := os.Rename(temporary, path); err != nil {
		return pkg, err
	}
	if err := os.Chmod(path, archiveFileMode); err != nil {
		return pkg, err
	}
	committed = true
	return pkg, nil
}

// writeArchivePayload 는 payloadDir 아래의 디렉터리와 일반 파일을 tar로 쓰고, 쓴 파일 크기의 합을 originalSize에
// 더한다. 서버가 읽지 못해 넣지 못한 항목의 payload 기준 경로를 돌려준다. payloadDir 는 os.Root로 열어 그 안에서만
// 읽는다(#176). 걷기(lstat)와 열기 사이에 에이전트가 파일을 링크로 바꿔도 payload 밖은 열지 않는다. 연 파일이 걷기에서
// 본 파일과 같은 일반 파일인지(os.SameFile) 다시 보고 아니면 오류로 끝내므로 파일을 payload 안의 다른 파일을 가리키는
// 링크로 바꿔도 그 내용이 엉뚱한 이름으로 들어가지 않는다. 디렉터리는 이 확인을 하지 않는다. 걷기는 디렉터리를 경로로
// 다시 열어 읽으므로, 디렉터리를 payload 안의 다른 디렉터리를 가리키는 링크로 바꾸면 그 디렉터리 내용이 바꾼 이름
// 아래에도 들어갈 수 있다. payload는 한 작업의 자료라 그 작업 밖의 내용이 들어가지는 않는다.
//
// 보관 형식은 처음부터 끝까지 일반 파일과 디렉터리만 지원한다(풀 때 다른 유형은 바로 오류다). 그래서 심볼릭 링크,
// FIFO, 장치 같은 항목은 패키지 전체를 실패시키지 않고 건너뛴다. 링크 대상은 읽지 않고 symlink 항목도 쓰지 않는다.
// 링크가 트리 안을 가리키면 대상 파일 자체는 따로 순회해 보관한다.
//
// 에이전트 uid 분리를 켜면 작업 공간 안의 에이전트 파일은 에이전트 uid 소유다. 에이전트가 권한을 바꾸면(chmod 600·000,
// setfacl) 서버는 그 파일을 읽거나 그 디렉터리를 나열하지 못한다(#179). 보관 전체를 실패시키면 그 작업은 영영 보관할 수
// 없으므로, 권한 오류로 읽지 못한 항목은 건너뛰고 돌려준다. 호출자가 보관 기록에 남긴다. 디렉터리를 나열하지 못하면
// 디렉터리 항목만 들어가고 그 안은 빠진다.
func writeArchivePayload(tarWriter *tar.Writer, payloadDir string, originalSize *int64) ([]string, error) {
	root, err := os.OpenRoot(payloadDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var skipped []string
	err = fs.WalkDir(root.FS(), ".", func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if current != "." && errors.Is(walkErr, fs.ErrPermission) {
				skipped = append(skipped, current)
				return nil
			}
			return walkErr
		}
		if current == "." {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrPermission) {
			skipped = append(skipped, current)
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			log.Printf("[task-archive] 일반 파일이 아닌 항목 건너뜀(보관 미지원, 다른 파일에는 영향 없음): %s", filepath.Join(payloadDir, filepath.FromSlash(current)))
			return nil
		}
		if info.IsDir() {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = current
			return tarWriter.WriteHeader(header)
		}
		// FIFO로 바꿔치기돼도 열기에서 멈추지 않게 O_NONBLOCK으로 연다. 일반 파일에는 영향이 없다.
		input, err := root.OpenFile(filepath.FromSlash(current), os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrPermission) {
			skipped = append(skipped, current)
			return nil
		}
		if err != nil {
			return err
		}
		defer input.Close()
		opened, err := input.Stat()
		if err != nil {
			return err
		}
		if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			// 건너뛰면 그 파일이 빠진 채 보관이 끝나고 원본이 지워진다. 실패시켜 보관 작업이 되돌리고 다시 하게 한다.
			return fmt.Errorf("archive payload entry changed while packaging: %s", current)
		}
		header, err := tar.FileInfoHeader(opened, "")
		if err != nil {
			return err
		}
		header.Name = current
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		// 연 뒤 파일이 커져도 헤더에 적은 크기만 쓴다(넘치면 tar 쓰기가 실패한다).
		if _, err := io.CopyN(tarWriter, input, header.Size); err != nil {
			return err
		}
		*originalSize += header.Size
		return nil
	})
	return skipped, err
}

func extractTaskArchivePackage(path, expectedSHA, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if expectedSHA != "" {
		hasher := sha256.New()
		if _, err := io.Copy(hasher, file); err != nil {
			return err
		}
		if hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(expectedSHA) {
			return errors.New("task archive checksum mismatch")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		return err
	}
	defer decoder.Close()
	tarReader := tar.NewReader(decoder)
	if err := os.MkdirAll(destination, archiveDirMode); err != nil {
		return err
	}
	var entries int
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveFiles || header.Size < 0 || total+header.Size > maxArchiveBytes {
			return errors.New("task archive exceeds extraction safety limits")
		}
		total += header.Size
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive path %q", header.Name)
		}
		target := filepath.Join(destination, clean)
		if relative, err := filepath.Rel(destination, target); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive target %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, archiveDirMode); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), archiveDirMode); err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, tarReader, header.Size)
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported task archive entry type %d", header.Typeflag)
		}
	}
	return nil
}
