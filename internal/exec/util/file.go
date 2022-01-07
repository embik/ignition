// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/flatcar-linux/ignition/internal/config/types"
	"github.com/flatcar-linux/ignition/internal/log"
	"github.com/flatcar-linux/ignition/internal/resource"
	"github.com/flatcar-linux/ignition/internal/util"
)

const (
	DefaultDirectoryPermissions os.FileMode = 0755
	DefaultFilePermissions      os.FileMode = 0644
)

type FetchOp struct {
	Hash         hash.Hash
	Path         string
	Url          url.URL
	Mode         *int
	FetchOptions resource.FetchOptions
	Overwrite    *bool
	Append       bool
	Node         types.Node
}

// newHashedReader returns a new ReadCloser that also writes to the provided hash.
func newHashedReader(reader io.ReadCloser, hasher hash.Hash) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{
		Reader: io.TeeReader(reader, hasher),
		Closer: reader,
	}
}

// PrepareFetch converts a given logger, http client, and types.File into a
// FetchOp. This includes operations such as parsing the source URL, generating
// a hasher, and performing user/group name lookups. If an error is encountered,
// the issue will be logged and nil will be returned.
func (u Util) PrepareFetch(l *log.Logger, f types.File) *FetchOp {
	var err error
	var expectedSum []byte

	// explicitly ignoring the error here because the config should already be
	// validated by this point
	uri, _ := url.Parse(f.Contents.Source)

	hasher, err := util.GetHasher(f.Contents.Verification)
	if err != nil {
		l.Crit("Error verifying file %q: %v", f.Path, err)
		return nil
	}

	if hasher != nil {
		// explicitly ignoring the error here because the config should already
		// be validated by this point
		_, expectedSumString, _ := util.HashParts(f.Contents.Verification)
		expectedSum, err = hex.DecodeString(expectedSumString)
		if err != nil {
			l.Crit("Error parsing verification string %q: %v", expectedSumString, err)
			return nil
		}
	}

	var headers http.Header
	if f.Contents.HTTPHeaders != nil && len(f.Contents.HTTPHeaders) > 0 {
		headers, err = f.Contents.HTTPHeaders.Parse()
		if err != nil {
			l.Crit("error parsing http headers: %v", err)
			return nil
		}
	}

	return &FetchOp{
		Path:      f.Path,
		Hash:      hasher,
		Node:      f.Node,
		Url:       *uri,
		Mode:      f.Mode,
		Overwrite: f.Overwrite,
		Append:    f.Append,
		FetchOptions: resource.FetchOptions{
			Hash:        hasher,
			Compression: f.Contents.Compression,
			ExpectedSum: expectedSum,
			Headers:     headers,
		},
	}
}

func (u Util) WriteLink(s types.Link) error {
	path, err := u.JoinPath(s.Path)
	if err != nil {
		return err
	}

	if err := MkdirForFile(path); err != nil {
		return err
	}

	if s.Hard {
		targetPath, err := u.JoinPath(s.Target)
		if err != nil {
			return err
		}
		return os.Link(targetPath, path)
	}

	if err := os.Symlink(s.Target, path); err != nil {
		return err
	}

	uid, gid, err := u.ResolveNodeUidAndGid(s.Node, 0, 0)
	if err != nil {
		return err
	}

	if err := os.Lchown(path, uid, gid); err != nil {
		return err
	}

	return nil
}

// PerformFetch performs a fetch operation generated by PrepareFetch, retrieving
// the file and writing it to disk. Any encountered errors are returned.
func (u Util) PerformFetch(f *FetchOp) error {
	path, err := u.JoinPath(string(f.Path))
	if err != nil {
		return err
	}

	if f.Overwrite != nil && *f.Overwrite == false {
		// Both directories and links will fail to be created if the target path
		// already exists. Because files are downloaded into a temporary file
		// and then renamed to the target path, we don't have the same
		// guarantees here. If the user explicitly doesn't want us to overwrite
		// preexisting nodes, check the target path and fail if something's
		// there.
		_, err := os.Lstat(path)
		switch {
		case os.IsNotExist(err):
			break
		case err != nil:
			return err
		default:
			return fmt.Errorf("error creating %q: something else exists at that path", f.Path)
		}
	}
	if f.Overwrite == nil && !f.Append {
		// For files, overwrite defaults to true if append is false. If
		// overwrite wasn't specified, delete the path.
		err := os.RemoveAll(path)
		if err != nil {
			return err
		}
	}

	if err := MkdirForFile(path); err != nil {
		return err
	}

	// Create a temporary file in the same directory to ensure it's on the same filesystem
	var tmp *os.File
	if tmp, err = ioutil.TempFile(filepath.Dir(path), "tmp"); err != nil {
		return err
	}

	defer tmp.Close()
	// sometimes the following line will fail (the file might be renamed),
	// but that's ok (we wanted to keep the file in that case).
	defer os.Remove(tmp.Name())

	err = u.Fetcher.Fetch(f.Url, tmp, f.FetchOptions)
	if err != nil {
		u.Crit("Error fetching file %q: %v", f.Path, err)
		return err
	}

	if f.Append {
		// Make sure that we're appending to a file
		finfo, err := os.Lstat(path)
		switch {
		case os.IsNotExist(err):
			// No problem, we'll create it.
			break
		case err != nil:
			return err
		default:
			if !finfo.Mode().IsRegular() {
				return fmt.Errorf("can only append to files: %q", f.Path)
			}
		}

		// Default to the appended file's owner for the uid and gid
		defaultUid, defaultGid, mode := getFileOwnerAndMode(path)
		uid, gid, err := u.ResolveNodeUidAndGid(f.Node, defaultUid, defaultGid)
		if err != nil {
			return err
		}
		if f.Mode != nil {
			mode = os.FileMode(*f.Mode)
		}

		targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, mode)
		if err != nil {
			return err
		}
		defer targetFile.Close()

		if _, err = tmp.Seek(0, os.SEEK_SET); err != nil {
			return err
		}
		if _, err = io.Copy(targetFile, tmp); err != nil {
			return err
		}

		if err = os.Chown(targetFile.Name(), uid, gid); err != nil {
			return err
		}
		if err = os.Chmod(targetFile.Name(), mode); err != nil {
			return err
		}
	} else {
		// XXX(vc): Note that we assume to be operating on the file we just wrote, this is only guaranteed
		// by using syscall.Fchown() and syscall.Fchmod()

		// Ensure the ownership and mode are as requested (since WriteFile can be affected by sticky bit)

		mode := os.FileMode(0)
		if f.Mode != nil {
			mode = os.FileMode(*f.Mode)
		}

		uid, gid, err := u.ResolveNodeUidAndGid(f.Node, 0, 0)
		if err != nil {
			return err
		}

		if err = os.Chown(tmp.Name(), uid, gid); err != nil {
			return err
		}

		if err = os.Chmod(tmp.Name(), mode); err != nil {
			return err
		}

		if err = os.Rename(tmp.Name(), path); err != nil {
			return err
		}
	}

	return nil
}

// MkdirForFile helper creates the directory components of path.
func MkdirForFile(path string) error {
	return os.MkdirAll(filepath.Dir(path), DefaultDirectoryPermissions)
}

// PathExists returns true if a node exists within DestDir, false otherwise. Any
// error other than ENOENT is treated as fatal.
func (u Util) PathExists(path string) (bool, error) {
	path, err := u.JoinPath(path)
	if err != nil {
		return false, err
	}

	_, err = os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// getFileOwner will return the uid and gid for the file at a given path. If the
// file doesn't exist, or some other error is encountered when running stat on
// the path, 0, 0, and 0 will be returned.
func getFileOwnerAndMode(path string) (int, int, os.FileMode) {
	finfo, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0
	}
	return int(finfo.Sys().(*syscall.Stat_t).Uid), int(finfo.Sys().(*syscall.Stat_t).Gid), finfo.Mode()
}

// ResolveNodeUidAndGid attempts to convert a types.Node into a concrete uid and
// gid. If the node has the User.ID field set, that's used for the uid. If the
// node has the User.Name field set, a username -> uid lookup is performed. If
// neither are set, it returns the passed in defaultUid. The logic is identical
// for gids with equivalent fields.
func (u Util) ResolveNodeUidAndGid(n types.Node, defaultUid, defaultGid int) (int, int, error) {
	var err error
	uid, gid := defaultUid, defaultGid
	if n.User != nil {
		if n.User.ID != nil {
			uid = *n.User.ID
		} else if n.User.Name != "" {
			uid, err = u.getUserID(n.User.Name)
			if err != nil {
				return 0, 0, err
			}
		}
	}
	if n.Group != nil {
		if n.Group.ID != nil {
			gid = *n.Group.ID
		} else if n.Group.Name != "" {
			gid, err = u.getGroupID(n.Group.Name)
			if err != nil {
				return 0, 0, err
			}
		}
	}
	return uid, gid, nil
}

func (u Util) getUserID(name string) (int, error) {
	usr, err := u.userLookup(name)
	if err != nil {
		return 0, fmt.Errorf("No such user %q: %v", name, err)
	}
	uid, err := strconv.ParseInt(usr.Uid, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("Couldn't parse uid %q: %v", usr.Uid, err)
	}
	return int(uid), nil
}

func (u Util) getGroupID(name string) (int, error) {
	g, err := u.groupLookup(name)
	if err != nil {
		return 0, fmt.Errorf("No such group %q: %v", name, err)
	}
	gid, err := strconv.ParseInt(g.Gid, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("Couldn't parse gid %q: %v", g.Gid, err)
	}
	return int(gid), nil
}

func (u Util) DeletePathOnOverwrite(n types.Node) error {
	if n.Overwrite == nil || !*n.Overwrite {
		return nil
	}
	path, err := u.JoinPath(string(n.Path))
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
