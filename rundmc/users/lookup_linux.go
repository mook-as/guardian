package users

import (
	"os"
	"path/filepath"

	"github.com/opencontainers/runc/libcontainer/user"
)

const (
	DefaultHome string = "/"
)

func LookupUser(rootFsPath, userName string) (*ExecUser, error) {
	defaultUser := &user.ExecUser{Uid: DefaultUID, Gid: DefaultGID, Home: DefaultHome}
	passwdPath := filepath.Join(rootFsPath, "etc", "passwd")

	_, err := os.Stat(passwdPath)
	if os.IsNotExist(err) && userName == "root" {
		return &ExecUser{Uid: DefaultUID, Gid: DefaultGID, Home: DefaultHome, Sgids: []int{}}, nil
	}

	execUser, err := user.GetExecUserPath(userName, defaultUser, passwdPath, "")
	if err != nil {
		return nil, err
	}

	return &ExecUser{Uid: execUser.Uid, Gid: execUser.Gid, Home: execUser.Home, Sgids: execUser.Sgids}, nil
}
