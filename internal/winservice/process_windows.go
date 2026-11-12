package winservice

import (
	"os"
	"os/user"
)

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	return p.Kill()
}

func currentUser() (id, name string, err error) {
	u, err := user.Current()
	if err != nil {
		return "", "", err
	}
	return u.Uid, u.Username, nil
}
