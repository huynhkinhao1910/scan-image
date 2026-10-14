package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// watchFile, beside the S3 settings, keeps the pushes still being looked at,
// so quitting the app does not end their watch.
const watchFile = "pushed.json"

// watched reports whether j must outlive the app: its object still has a look
// ahead, or it is being sent again after one found the object replaced.
func watched(j *uploadJob) bool {
	return recheckPending(j) || j.Repush > 0 && (j.State == upQueued || j.State == upRunning)
}

// encodeWatch is the watch file for jobs. The secret key is not in it.
func encodeWatch(jobs []*uploadJob) ([]byte, error) {
	keep := []*uploadJob{}
	for _, j := range jobs {
		if watched(j) {
			keep = append(keep, j)
		}
	}
	return json.Marshal(keep)
}

// decodeWatch reads a watch file back into finished jobs, each with the
// secret of its access key. A job that was cut off while being sent again is
// due for a look at once, which sends it again if the object is still wrong.
// Counters are clamped: the file is not trusted to index recheckDelays.
func decodeWatch(b []byte, secret func(accessKey string) string) ([]*uploadJob, error) {
	var saved []*uploadJob
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, err
	}
	secrets := map[string]string{}
	var jobs []*uploadJob
	for _, j := range saved {
		if j == nil || j.Key == "" || j.Path == "" {
			continue
		}
		if j.State != upDone {
			j.Checks, j.Done = 0, time.Time{}
		}
		j.Checks, j.Repush = min(max(j.Checks, 0), len(recheckDelays)), max(j.Repush, 0)
		j.State, j.Sent, j.Err = upDone, j.Size, ""
		ak := j.Cfg.AccessKey
		if _, ok := secrets[ak]; !ok {
			secrets[ak] = secret(ak)
		}
		j.Cfg.SecretKey = secrets[ak]
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// secretFor returns the secret key of accessKey: the loaded one, else the
// Keychain's.
func (u *uploads) secretFor(accessKey string) string {
	if accessKey == u.cfg.AccessKey {
		return u.cfg.SecretKey
	}
	if u.secrets == nil {
		return ""
	}
	s, _ := u.secrets.Get(accessKey)
	return s
}

func (u *uploads) watchPath() string { return filepath.Join(filepath.Dir(u.cfgPath), watchFile) }

// saveWatch writes the watched pushes to local disk when they changed.
// UI thread only.
func (a *app) saveWatch() {
	u := &a.up
	if u.cfgPath == "" {
		return
	}
	b, err := encodeWatch(u.jobs)
	if err == nil && !bytes.Equal(b, u.recheck.saved) {
		if err = writeFileAtomic(u.watchPath(), b); err == nil {
			u.recheck.saved = b
		}
	}
	if err != nil {
		a.status = "Không lưu được danh sách file đang theo dõi trên S3: " + oneLine(err)
	}
}

// restoreWatch takes up the watch of the pushes an earlier run was still
// looking at. A file that cannot be decoded is set aside as ".bad" rather
// than overwritten by the next save. UI thread only.
func (a *app) restoreWatch() {
	u := &a.up
	if u.cfgPath == "" {
		return
	}
	b, err := os.ReadFile(u.watchPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		a.status = "Không đọc được danh sách file đang theo dõi trên S3: " + oneLine(err)
		return
	}
	jobs, err := decodeWatch(b, u.secretFor)
	if err != nil {
		a.status = "Danh sách file đang theo dõi trên S3 bị hỏng, đã đổi tên thành " + watchFile + ".bad: " + oneLine(err)
		if err := os.Rename(u.watchPath(), u.watchPath()+".bad"); err != nil {
			a.status += " (" + oneLine(err) + ")"
		}
		return
	}
	u.recheck.saved = b
	if len(jobs) == 0 {
		return
	}
	u.jobs = append(u.jobs, jobs...)
	a.startUploader(u.cfg)
	a.recheck()
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
