package documents

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	pdfinfo     = "/usr/bin/pdfinfo"
	pdftotext   = "/usr/bin/pdftotext"
	pdftoppm    = "/usr/bin/pdftoppm"
	tesseract   = "/usr/bin/tesseract"
	libreoffice = "/usr/lib/libreoffice/program/soffice.bin"
	prlimit     = "/usr/bin/prlimit"
)

// Capabilities reports fixed-path executable availability, not document support,
// successful startup, sandboxing, or the integrity of the installed tools.
type Capabilities struct{ PDF, OCR, Office bool }

// ProbeCapabilities checks local executables and standard English OCR data paths.
// It does not invoke programs or inspect environment PATH.
func ProbeCapabilities() Capabilities {
	exists := func(path string) bool {
		s, err := os.Stat(path)
		return err == nil && s.Mode().IsRegular() && s.Mode().Perm()&0111 != 0
	}
	pdf := exists(prlimit) && exists(pdfinfo) && exists(pdftotext)
	_, engErr := os.Stat("/usr/share/tesseract-ocr/5/tessdata/eng.traineddata")
	if engErr != nil {
		_, engErr = os.Stat("/usr/share/tessdata/eng.traineddata")
	}
	return Capabilities{PDF: pdf, OCR: pdf && exists(pdftoppm) && exists(tesseract) && engErr == nil, Office: pdf && exists(libreoffice)}
}

type boundedOutput struct {
	bytes.Buffer
	exceeded bool
	onLimit  func()
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxText-b.Len() {
		b.exceeded = true
		if b.onLimit != nil {
			b.onLimit()
		}
		return 0, ErrLimit
	}
	return b.Buffer.Write(p)
}

// RLIMIT_FSIZE is a hard per-file limit inherited by children. Aggregate temp
// usage is monitored every 25ms (not a filesystem quota, so it may overshoot).
// These controls do not isolate filesystem/network access or escaped sessions.
func runProcess(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
	switch program {
	case pdfinfo, pdftotext, pdftoppm, tesseract, libreoffice:
	default:
		return nil, ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkDisk(dir); err != nil {
		return nil, err
	}
	cmd := processCommand(ctx, dir, program, args...)
	var out boundedOutput
	out.onLimit = func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if out.exceeded {
				return nil, ErrLimit
			}
			if diskErr := checkDisk(dir); diskErr != nil {
				return nil, diskErr
			}
			return out.Bytes(), err
		case <-ticker.C:
			if err := checkDisk(dir); err != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				return nil, err
			}
		}
	}
}

func processCommand(ctx context.Context, dir, program string, args ...string) *exec.Cmd {
	limits := []string{"--fsize=33554432:33554432", "--as=1073741824:1073741824", "--cpu=150:150", "--nofile=128:128", "--core=0:0", "--", program}
	cmd := exec.CommandContext(ctx, prlimit, append(limits, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_CACHE_HOME=" + dir, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "SAL_USE_VCLPLUGIN=svp", "OMP_THREAD_LIMIT=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd
}

func checkDisk(dir string) error {
	var total int64
	count := 0
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		count++
		if count > 2048 {
			return ErrLimit
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrInvalid
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrInvalid
		}
		total += info.Size()
		if info.Size() > maxFile || total > maxDisk {
			return ErrLimit
		}
		return nil
	})
}
