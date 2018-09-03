// Copyright 2011 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package tailer

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/google/go-cmp/cmp"
	"github.com/google/mtail/watcher"

	"github.com/spf13/afero"
)

func makeTestTail(t *testing.T) (*Tailer, chan *LogLine, *watcher.FakeWatcher, afero.Fs, string, func()) {
	fs := afero.NewMemMapFs()
	w := watcher.NewFakeWatcher()
	lines := make(chan *LogLine, 1)
	ta, err := New(lines, fs, w)
	if err != nil {
		t.Fatal(err)
	}
	fs.Mkdir("tail_test", os.ModePerm)
	return ta, lines, w, fs, "/tail_test", func() {}
}

func makeTestTailReal(t *testing.T, prefix string) (*Tailer, chan *LogLine, *watcher.LogWatcher, afero.Fs, string, func()) {
	if testing.Short() {
		t.Skip("skipping real fs test in short mode")
	}
	dir, err := ioutil.TempDir("", prefix)
	if err != nil {
		t.Fatalf("can't create tempdir: %v", err)
	}

	fs := afero.NewOsFs()
	w, err := watcher.NewLogWatcher()
	if err != nil {
		t.Fatalf("can't create watcher: %v", err)
	}
	lines := make(chan *LogLine, 1)
	ta, err := New(lines, fs, w)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Log(err)
		}
	}
	return ta, lines, w, fs, dir, cleanup
}

func TestTail(t *testing.T) {
	ta, _, w, fs, dir, cleanup := makeTestTail(t)
	defer cleanup()

	logfile := filepath.Join(dir, "log")
	f, err := fs.Create(logfile)
	if err != nil {
		t.Error(err)
	}
	defer f.Close()
	defer w.Close()

	err = ta.TailPath(logfile)
	if err != nil {
		t.Fatal(err)
	}
	// Tail also causes the log to be read, so no need to inject an event.

	if _, ok := ta.handles[logfile]; !ok {
		t.Errorf("path not found in files map: %+#v", ta.handles)
	}
}

func TestHandleLogUpdate(t *testing.T) {
	ta, lines, w, fs, dir, cleanup := makeTestTail(t)
	defer cleanup()

	logfile := filepath.Join(dir, "log")
	f, err := fs.Create(logfile)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	result := []*LogLine{}
	done := make(chan struct{})
	wg := sync.WaitGroup{}
	go func() {
		for line := range lines {
			result = append(result, line)
			wg.Done()
		}
		close(done)
	}()

	err = ta.TailPath(logfile)
	if err != nil {
		t.Fatal(err)
	}

	wg.Add(4)
	_, err = f.WriteString("a\nb\nc\nd\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Seek(0, 0) // afero in-memory files share the same offset
	w.InjectUpdate(logfile)

	wg.Wait()
	if err := w.Close(); err != nil {
		t.Log(err)
	}
	<-done

	expected := []*LogLine{
		{logfile, "a"},
		{logfile, "b"},
		{logfile, "c"},
		{logfile, "d"},
	}
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result didn't match:\n%s", diff)
	}
}

// TestHandleLogTruncate writes to a file, waits for those
// writes to be seen, then truncates the file and writes some more.
// At the end all lines written must be reported by the tailer.
func TestHandleLogTruncate(t *testing.T) {
	ta, lines, w, fs, dir, cleanup := makeTestTailReal(t, "truncate")
	defer cleanup()

	logfile := filepath.Join(dir, "log")
	f, err := fs.Create(logfile)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	result := []*LogLine{}
	done := make(chan struct{})
	wg := sync.WaitGroup{}
	go func() {
		for line := range lines {
			result = append(result, line)
			wg.Done()
		}
		close(done)
	}()

	if err = ta.TailPath(logfile); err != nil {
		t.Fatal(err)
	}

	wg.Add(3)
	if _, err = f.WriteString("a\nb\nc\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	wg.Wait()

	if err = f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	// "File.Truncate" does not change the file offset.
	f.Seek(0, 0)
	time.Sleep(10 * time.Millisecond)

	wg.Add(2)
	if _, err = f.WriteString("d\ne\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	wg.Wait()
	if err := w.Close(); err != nil {
		t.Log(err)
	}
	<-done

	expected := []*LogLine{
		{logfile, "a"},
		{logfile, "b"},
		{logfile, "c"},
		{logfile, "d"},
		{logfile, "e"},
	}
	if diff := cmp.Diff(expected, result); diff != "" {
		t.Errorf("result didn't match:\n%s", diff)
	}
}

func TestHandleLogUpdatePartialLine(t *testing.T) {
	ta, lines, w, fs, dir, cleanup := makeTestTail(t)
	defer cleanup()

	logfile := filepath.Join(dir, "log")
	f, err := fs.Create(logfile)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	result := []*LogLine{}
	done := make(chan struct{})
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		for line := range lines {
			result = append(result, line)
			wg.Done()
		}
		close(done)
	}()

	err = ta.TailPath(logfile)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.WriteString("a")
	if err != nil {
		t.Fatal(err)
	}
	f.Seek(0, 0)
	w.InjectUpdate(logfile)

	f.Seek(1, 0)
	_, err = f.WriteString("b")
	if err != nil {
		t.Error(err)
	}
	f.Seek(1, 0)
	w.InjectUpdate(logfile)

	f.Seek(2, 0)
	_, err = f.WriteString("\n")
	if err != nil {
		t.Error(err)
	}
	f.Seek(2, 0)
	w.InjectUpdate(logfile)

	wg.Wait()
	w.Close()
	<-done

	expected := []*LogLine{
		{logfile, "ab"},
	}
	diff := cmp.Diff(expected, result)
	if diff != "" {
		t.Errorf("result didn't match:\n%s", diff)
	}

}

func TestReadPartial(t *testing.T) {
	ta, lines, w, fs, _, cleanup := makeTestTail(t)
	defer cleanup()
	defer w.Close()

	f, err := fs.Create("t")
	if err != nil {
		t.Fatal(err)
	}
	p := bytes.NewBufferString("")
	err = ta.read(f, p)
	if p.String() != "" {
		t.Errorf("partial line returned not empty: %q", p)
	}
	if err != io.EOF {
		t.Errorf("error returned not EOF: %v", err)
	}
	p.Reset()
	p.WriteString("o")
	f.WriteString("hi")
	f.Seek(0, 0)
	err = ta.read(f, p)
	if p.String() != "ohi" {
		t.Errorf("partial line returned not expected: %q", p)
	}
	if err != io.EOF {
		t.Errorf("error returned not EOF: %v", err)
	}
	p.Reset()
	err = ta.read(f, p)
	if err != io.EOF {
		t.Errorf("error returned not EOF: %v", err)
	}
	f.WriteString("\n")
	f.Seek(-1, io.SeekEnd)
	p.Reset()
	p.WriteString("ohi")
	err = ta.read(f, p)
	l := <-lines
	if l.Line != "ohi" {
		t.Errorf("line emitted not ohi: %q", l)
	}
	if p.String() != "" {
		t.Errorf("partial not empty: %q", p)
	}
	if err != io.EOF {
		t.Errorf("error returned not EOF: %v", err)
	}
}

func TestOpenRetries(t *testing.T) {
	// Can't force a permission denied error if run as root.
	u, err := user.Current()
	if err != nil {
		t.Skip(fmt.Sprintf("Couldn't determine current user id: %s", err))
	}
	if u.Uid == "0" {
		t.Skip("Skipping test when run as root")
	}
	// Use the real filesystem because afero doesn't implement correct
	// permissions checking on OpenFile in the memfile implementation.
	ta, lines, w, fs, dir, cleanup := makeTestTailReal(t, "retries")
	defer cleanup()

	logfile := filepath.Join(dir, "log")
	if _, err := fs.OpenFile(logfile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	wg := sync.WaitGroup{}
	wg.Add(1) // lines written
	go func() {
		for range lines {
			wg.Done()
		}
		close(done)
	}()
	ta.AddPattern(logfile)

	if err := ta.TailPath(logfile); err == nil || !os.IsPermission(err) {
		t.Fatalf("Expected a permission denied error here: %s", err)
	}
	time.Sleep(10 * time.Millisecond)
	glog.Info("remove")
	if err := fs.Remove(logfile); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	glog.Info("openfile")
	f, err := fs.OpenFile(logfile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	glog.Info("chmod")
	if err := fs.Chmod(logfile, 0666); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	glog.Info("write string")
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Log(err)
	}
	<-done
}

func TestTailerInitErrors(t *testing.T) {
	_, err := New(nil, nil, nil)
	if err == nil {
		t.Error("expected error")
	}
	lines := make(chan *LogLine)
	_, err = New(lines, nil, nil)
	if err == nil {
		t.Error("expected error")
	}
	fs := afero.NewMemMapFs()
	_, err = New(lines, fs, nil)
	if err == nil {
		t.Error("expected error")
	}
	w := watcher.NewFakeWatcher()
	_, err = New(lines, fs, w)
	if err != nil {
		t.Errorf("unexpected error %s", err)
	}
	_, err = New(lines, fs, w, OneShot)
	if err != nil {
		t.Errorf("unexpected error %s", err)
	}
}

func TestHandleLogRotate(t *testing.T) {
	ta, lines, w, fs, dir, cleanup := makeTestTailReal(t, "rotate")
	defer cleanup()

	logfile := filepath.Join(dir, "log")
	f, err := fs.Create(logfile)
	if err != nil {
		t.Fatal(err)
	}

	result := []*LogLine{}
	done := make(chan struct{})
	wg := sync.WaitGroup{}
	go func() {
		for line := range lines {
			result = append(result, line)
			wg.Done()
		}
		close(done)
	}()

	if err := ta.TailPath(logfile); err != nil {
		t.Fatal(err)
	}
	wg.Add(2)
	if _, err = f.WriteString("1\n"); err != nil {
		t.Fatal(err)
	}
	glog.V(2).Info("update")
	//w.InjectUpdate(logfile)
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = fs.Rename(logfile, logfile+".1"); err != nil {
		t.Fatal(err)
	}
	glog.V(2).Info("delete")
	//w.InjectDelete(logfile)
	//w.InjectCreate(logfile + ".1")
	f, err = fs.Create(logfile)
	if err != nil {
		t.Fatal(err)
	}
	glog.V(2).Info("create")
	//w.InjectCreate(logfile)
	if _, err = f.WriteString("2\n"); err != nil {
		t.Fatal(err)
	}
	glog.V(2).Info("update")
	//w.InjectUpdate(logfile)

	wg.Wait()
	w.Close()
	<-done

	expected := []*LogLine{
		{logfile, "1"},
		{logfile, "2"},
	}
	diff := cmp.Diff(expected, result)
	if diff != "" {
		t.Errorf("result didn't match expected:\n%s", diff)
	}
}

// func TestHandleLogRotateSignalsWrong(t *testing.T) {
// 	ta, lines, w, fs, dir, cleanup := makeTestTailReal(t, "rotate wrong")
// 	defer cleanup()
// 	logfile := filepath.Join(dir, "log")
// 	f, err := fs.Create(logfile)
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	glog.V(2).Infof("Fileinfo: %#V", f.(*mem.File).Info())

// 	result := []*LogLine{}
// 	done := make(chan struct{})
// 	wg := sync.WaitGroup{}
// 	go func() {
// 		for line := range lines {
// 			result = append(result, line)
// 			wg.Done()
// 		}
// 		close(done)
// 	}()

// 	if err := ta.TailPath(logfile); err != nil {
// 		t.Fatal(err)
// 	}
// 	wg.Add(2)
// 	if _, err = f.WriteString("1\n"); err != nil {
// 		t.Fatal(err)
// 	}
// 	glog.V(2).Info("update")
// 	w.InjectUpdate(logfile)
// 	if err = f.Close(); err != nil {
// 		t.Fatal(err)
// 	}
// 	if err = fs.Rename(logfile, logfile+".1"); err != nil {
// 		t.Fatal(err)
// 	}
// 	glog.V(2).Infof("Fileinfo: %#V", f.(*mem.File).Info())
// 	// Forcibly remove it from the fake filesystem because afero bugs
// 	fs.Remove(logfile)
// 	glog.V(2).Infof("Fileinfo: %#V", f.(*mem.File).Info())
// 	// No delete signal yet
// 	f, err = fs.Create(logfile)
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	glog.V(2).Infof("Fileinfo: %#V", f.(*mem.File).Info())
// 	glog.V(2).Info("create")
// 	// Out-of-order delivery of a log rotation.
// 	w.InjectCreate(logfile)

// 	time.Sleep(1 * time.Millisecond)
// 	glog.V(2).Info("delete")
// 	w.InjectDelete(logfile)

// 	if _, err = f.WriteString("2\n"); err != nil {
// 		t.Fatal(err)
// 	}
// 	glog.V(2).Info("update")
// 	w.InjectUpdate(logfile)

// 	wg.Wait()
// 	w.Close()
// 	<-done

// 	expected := []*LogLine{
// 		{logfile, "1"},
// 		{logfile, "2"},
// 	}
// 	diff := cmp.Diff(expected, result)
// 	if diff != "" {
// 		t.Errorf("result didn't match expected:\n%s", diff)
// 	}
// }
