// Package swift provides an interface to the Swift object storage system
package swift

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/config/configmap"
	"github.com/ncw/rclone/fs/config/configstruct"
	"github.com/ncw/rclone/fs/fserrors"
	"github.com/ncw/rclone/fs/fshttp"
	"github.com/ncw/rclone/fs/hash"
	"github.com/ncw/rclone/fs/operations"
	"github.com/ncw/rclone/fs/walk"
	"github.com/ncw/swift"
	"github.com/pkg/errors"
)

// Constants
const (
	directoryMarkerContentType = "application/directory" // content type of directory marker objects
	listChunks                 = 1000                    // chunk size to read directory listings
	defaultChunkSize           = 5 * fs.GibiByte
)

// SharedOptions are shared between swift and hubic
var SharedOptions = []fs.Option{{
	Name: "chunk_size",
	Help: `Above this size files will be chunked into a _segments container.

Above this size files will be chunked into a _segments container.  The
default for this is 5GB which is its maximum value.`,
	Default:  defaultChunkSize,
	Advanced: true,
}}

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "swift",
		Description: "Openstack Swift (Rackspace Cloud Files, Memset Memstore, OVH)",
		NewFs:       NewFs,
		Options: append([]fs.Option{{
			Name:    "env_auth",
			Help:    "Get swift credentials from environment variables in standard OpenStack form. (SWIFT_ENV_AUTH)",
			Default: false,
			Examples: []fs.OptionExample{
				{
					Value: "false",
					Help:  "Enter swift credentials in the next step",
				}, {
					Value: "true",
					Help:  "Get swift credentials from environment vars. Leave other fields blank if using this.",
				},
			},
		}, {
			Name: "user",
			Help: "User name to log in (OS_USERNAME).",
		}, {
			Name: "key",
			Help: "API key or password (OS_PASSWORD).",
		}, {
			Name: "auth",
			Help: "Authentication URL for server (OS_AUTH_URL).",
			Examples: []fs.OptionExample{{
				Help:  "Rackspace US",
				Value: "https://auth.api.rackspacecloud.com/v1.0",
			}, {
				Help:  "Rackspace UK",
				Value: "https://lon.auth.api.rackspacecloud.com/v1.0",
			}, {
				Help:  "Rackspace v2",
				Value: "https://identity.api.rackspacecloud.com/v2.0",
			}, {
				Help:  "Memset Memstore UK",
				Value: "https://auth.storage.memset.com/v1.0",
			}, {
				Help:  "Memset Memstore UK v2",
				Value: "https://auth.storage.memset.com/v2.0",
			}, {
				Help:  "OVH",
				Value: "https://auth.cloud.ovh.net/v2.0",
			}},
		}, {
			Name: "user_id",
			Help: "User ID to log in - optional - most swift systems use user and leave this blank (v3 auth) (OS_USER_ID).",
		}, {
			Name: "domain",
			Help: "User domain - optional (v3 auth) (OS_USER_DOMAIN_NAME)",
		}, {
			Name: "tenant",
			Help: "Tenant name - optional for v1 auth, this or tenant_id required otherwise (OS_TENANT_NAME or OS_PROJECT_NAME)",
		}, {
			Name: "tenant_id",
			Help: "Tenant ID - optional for v1 auth, this or tenant required otherwise (OS_TENANT_ID)",
		}, {
			Name: "tenant_domain",
			Help: "Tenant domain - optional (v3 auth) (OS_PROJECT_DOMAIN_NAME)",
		}, {
			Name: "region",
			Help: "Region name - optional (OS_REGION_NAME)",
		}, {
			Name: "storage_url",
			Help: "Storage URL - optional (OS_STORAGE_URL)",
		}, {
			Name: "auth_token",
			Help: "Auth Token from alternate authentication - optional (OS_AUTH_TOKEN)",
		}, {
			Name:    "auth_version",
			Help:    "AuthVersion - optional - set to (1,2,3) if your auth URL has no version (ST_AUTH_VERSION)",
			Default: 0,
		}, {
			Name:    "endpoint_type",
			Help:    "Endpoint type to choose from the service catalogue (OS_ENDPOINT_TYPE)",
			Default: "public",
			Examples: []fs.OptionExample{{
				Help:  "Public (default, choose this if not sure)",
				Value: "public",
			}, {
				Help:  "Internal (use internal service net)",
				Value: "internal",
			}, {
				Help:  "Admin",
				Value: "admin",
			}},
		}, {
			Name: "storage_policy",
			Help: `The storage policy to use when creating a new container

This applies the specified storage policy when creating a new
container. The policy cannot be changed afterwards. The allowed
configuration values and their meaning depend on your Swift storage
provider.`,
			Default: "",
			Examples: []fs.OptionExample{{
				Help:  "Default",
				Value: "",
			}, {
				Help:  "OVH Public Cloud Storage",
				Value: "pcs",
			}, {
				Help:  "OVH Public Cloud Archive",
				Value: "pca",
			}},
		}}, SharedOptions...),
	})
}

// Options defines the configuration for this backend
type Options struct {
	EnvAuth       bool          `config:"env_auth"`
	User          string        `config:"user"`
	Key           string        `config:"key"`
	Auth          string        `config:"auth"`
	UserID        string        `config:"user_id"`
	Domain        string        `config:"domain"`
	Tenant        string        `config:"tenant"`
	TenantID      string        `config:"tenant_id"`
	TenantDomain  string        `config:"tenant_domain"`
	Region        string        `config:"region"`
	StorageURL    string        `config:"storage_url"`
	AuthToken     string        `config:"auth_token"`
	AuthVersion   int           `config:"auth_version"`
	StoragePolicy string        `config:"storage_policy"`
	EndpointType  string        `config:"endpoint_type"`
	ChunkSize     fs.SizeSuffix `config:"chunk_size"`
}

// Fs represents a remote swift server
type Fs struct {
	name              string            // name of this remote
	root              string            // the path we are working on if any
	features          *fs.Features      // optional features
	opt               Options           // options for this backend
	c                 *swift.Connection // the connection to the swift server
	container         string            // the container we are working on
	containerOKMu     sync.Mutex        // mutex to protect container OK
	containerOK       bool              // true if we have created the container
	segmentsContainer string            // container to store the segments (if any) in
	noCheckContainer  bool              // don't check the container before creating it
}

// Object describes a swift object
//
// Will definitely have info but maybe not meta
type Object struct {
	fs      *Fs           // what this object is part of
	remote  string        // The remote path
	info    swift.Object  // Info from the swift object if known
	headers swift.Headers // The object headers if known
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	if f.root == "" {
		return f.container
	}
	return f.container + "/" + f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	if f.root == "" {
		return fmt.Sprintf("Swift container %s", f.container)
	}
	return fmt.Sprintf("Swift container %s path %s", f.container, f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Pattern to match a swift path
var matcher = regexp.MustCompile(`^/*([^/]*)(.*)$`)

// parseParse parses a swift 'url'
func parsePath(path string) (container, directory string, err error) {
	parts := matcher.FindStringSubmatch(path)
	if parts == nil {
		err = errors.Errorf("couldn't find container in swift path %q", path)
	} else {
		container, directory = parts[1], parts[2]
		directory = strings.Trim(directory, "/")
	}
	return
}

// swiftConnection makes a connection to swift
func swiftConnection(opt *Options, name string) (*swift.Connection, error) {
	c := &swift.Connection{
		// Keep these in the same order as the Config for ease of checking
		UserName:       opt.User,
		ApiKey:         opt.Key,
		AuthUrl:        opt.Auth,
		UserId:         opt.UserID,
		Domain:         opt.Domain,
		Tenant:         opt.Tenant,
		TenantId:       opt.TenantID,
		TenantDomain:   opt.TenantDomain,
		Region:         opt.Region,
		StorageUrl:     opt.StorageURL,
		AuthToken:      opt.AuthToken,
		AuthVersion:    opt.AuthVersion,
		EndpointType:   swift.EndpointType(opt.EndpointType),
		ConnectTimeout: 10 * fs.Config.ConnectTimeout, // Use the timeouts in the transport
		Timeout:        10 * fs.Config.Timeout,        // Use the timeouts in the transport
		Transport:      fshttp.NewTransport(fs.Config),
	}
	if opt.EnvAuth {
		err := c.ApplyEnvironment()
		if err != nil {
			return nil, errors.Wrap(err, "failed to read environment variables")
		}
	}
	StorageUrl, AuthToken := c.StorageUrl, c.AuthToken // nolint
	if !c.Authenticated() {
		if c.UserName == "" && c.UserId == "" {
			return nil, errors.New("user name or user id not found for authentication (and no storage_url+auth_token is provided)")
		}
		if c.ApiKey == "" {
			return nil, errors.New("key not found")
		}
		if c.AuthUrl == "" {
			return nil, errors.New("auth not found")
		}
		err := c.Authenticate() // fills in c.StorageUrl and c.AuthToken
		if err != nil {
			return nil, err
		}
	}
	// Make sure we re-auth with the AuthToken and StorageUrl
	// provided by wrapping the existing auth, so we can just
	// override one or the other or both.
	if StorageUrl != "" || AuthToken != "" {
		// Re-write StorageURL and AuthToken if they are being
		// overridden as c.Authenticate above will have
		// overwritten them.
		if StorageUrl != "" {
			c.StorageUrl = StorageUrl
		}
		if AuthToken != "" {
			c.AuthToken = AuthToken
		}
		c.Auth = newAuth(c.Auth, StorageUrl, AuthToken)
	}
	return c, nil
}

func checkUploadChunkSize(cs fs.SizeSuffix) error {
	const minChunkSize = fs.Byte
	if cs < minChunkSize {
		return errors.Errorf("%s is less than %s", cs, minChunkSize)
	}
	return nil
}

func (f *Fs) setUploadChunkSize(cs fs.SizeSuffix) (old fs.SizeSuffix, err error) {
	err = checkUploadChunkSize(cs)
	if err == nil {
		old, f.opt.ChunkSize = f.opt.ChunkSize, cs
	}
	return
}

// NewFsWithConnection constructs an Fs from the path, container:path
// and authenticated connection.
//
// if noCheckContainer is set then the Fs won't check the container
// exists before creating it.
func NewFsWithConnection(opt *Options, name, root string, c *swift.Connection, noCheckContainer bool) (fs.Fs, error) {
	container, directory, err := parsePath(root)
	if err != nil {
		return nil, err
	}
	f := &Fs{
		name:              name,
		opt:               *opt,
		c:                 c,
		container:         container,
		segmentsContainer: container + "_segments",
		root:              directory,
		noCheckContainer:  noCheckContainer,
	}
	f.features = (&fs.Features{
		ReadMimeType:  true,
		WriteMimeType: true,
		BucketBased:   true,
	}).Fill(f)
	if f.root != "" {
		f.root += "/"
		// Check to see if the object exists - ignoring directory markers
		info, _, err := f.c.Object(container, directory)
		if err == nil && info.ContentType != directoryMarkerContentType {
			f.root = path.Dir(directory)
			if f.root == "." {
				f.root = ""
			} else {
				f.root += "/"
			}
			// return an error with an fs which points to the parent
			return f, fs.ErrorIsFile
		}
	}
	return f, nil
}

// NewFs constructs an Fs from the path, container:path
func NewFs(name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}
	err = checkUploadChunkSize(opt.ChunkSize)
	if err != nil {
		return nil, errors.Wrap(err, "swift: chunk size")
	}

	//Check if set - ENV takes precedence
	val, ok := os.LookupEnv("SWIFT_ENV_AUTH")
	if ok {
		envAuth, err := strconv.ParseBool(val)
		if err != nil {
			return nil, errors.Errorf("SWIFT_ENV_AUTH: %s", err.Error())
		}
		opt.EnvAuth = envAuth
	}

	c, err := swiftConnection(opt, name)
	if err != nil {
		return nil, err
	}
	return NewFsWithConnection(opt, name, root, c, false)
}

// Return an Object from a path
//
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) newObjectWithInfo(remote string, info *swift.Object) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	// Note that due to a quirk of swift, dynamic large objects are
	// returned as 0 bytes in the listing.  Correct this here by
	// making sure we read the full metadata for all 0 byte files.
	// We don't read the metadata for directory marker objects.
	if info != nil && info.Bytes == 0 && info.ContentType != "application/directory" {
		info = nil
	}
	if info != nil {
		// Set info but not headers
		o.info = *info
	} else {
		err := o.readMetaData() // reads info and headers, returning an error
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

// NewObject finds the Object at remote.  If it can't be found it
// returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(remote string) (fs.Object, error) {
	return f.newObjectWithInfo(remote, nil)
}

// listFn is called from list and listContainerRoot to handle an object.
type listFn func(remote string, object *swift.Object, isDirectory bool) error

// listContainerRoot lists the objects into the function supplied from
// the container and root supplied
//
// Set recurse to read sub directories
func (f *Fs) listContainerRoot(container, root string, dir string, recurse bool, fn listFn) error {
	prefix := root
	if dir != "" {
		prefix += dir + "/"
	}
	// Options for ObjectsWalk
	opts := swift.ObjectsOpts{
		Prefix: prefix,
		Limit:  listChunks,
	}
	if !recurse {
		opts.Delimiter = '/'
	}
	rootLength := len(root)
	return f.c.ObjectsWalk(container, &opts, func(opts *swift.ObjectsOpts) (interface{}, error) {
		objects, err := f.c.Objects(container, opts)
		if err == nil {
			for i := range objects {
				object := &objects[i]
				isDirectory := false
				if !recurse {
					isDirectory = strings.HasSuffix(object.Name, "/")
				}
				if !strings.HasPrefix(object.Name, prefix) {
					fs.Logf(f, "Odd name received %q", object.Name)
					continue
				}
				if object.Name == prefix {
					// If we have zero length directory markers ending in / then swift
					// will return them in the listing for the directory which causes
					// duplicate directories.  Ignore them here.
					continue
				}
				remote := object.Name[rootLength:]
				err = fn(remote, object, isDirectory)
				if err != nil {
					break
				}
			}
		}
		return objects, err
	})
}

type addEntryFn func(fs.DirEntry) error

// list the objects into the function supplied
func (f *Fs) list(dir string, recurse bool, fn addEntryFn) error {
	err := f.listContainerRoot(f.container, f.root, dir, recurse, func(remote string, object *swift.Object, isDirectory bool) (err error) {
		if isDirectory {
			remote = strings.TrimRight(remote, "/")
			d := fs.NewDir(remote, time.Time{}).SetSize(object.Bytes)
			err = fn(d)
		} else {
			// newObjectWithInfo does a full metadata read on 0 size objects which might be dynamic large objects
			var o fs.Object
			o, err = f.newObjectWithInfo(remote, object)
			if err != nil {
				return err
			}
			if o.Storable() {
				err = fn(o)
			}
		}
		return err
	})
	if err == swift.ContainerNotFound {
		err = fs.ErrorDirNotFound
	}
	return err
}

// mark the container as being OK
func (f *Fs) markContainerOK() {
	if f.container != "" {
		f.containerOKMu.Lock()
		f.containerOK = true
		f.containerOKMu.Unlock()
	}
}

// listDir lists a single directory
func (f *Fs) listDir(dir string) (entries fs.DirEntries, err error) {
	if f.container == "" {
		return nil, fs.ErrorListBucketRequired
	}
	// List the objects
	err = f.list(dir, false, func(entry fs.DirEntry) error {
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// container must be present if listing succeeded
	f.markContainerOK()
	return entries, nil
}

// listContainers lists the containers
func (f *Fs) listContainers(dir string) (entries fs.DirEntries, err error) {
	if dir != "" {
		return nil, fs.ErrorListBucketRequired
	}
	containers, err := f.c.ContainersAll(nil)
	if err != nil {
		return nil, errors.Wrap(err, "container listing failed")
	}
	for _, container := range containers {
		d := fs.NewDir(container.Name, time.Time{}).SetSize(container.Bytes).SetItems(container.Count)
		entries = append(entries, d)
	}
	return entries, nil
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(dir string) (entries fs.DirEntries, err error) {
	if f.container == "" {
		return f.listContainers(dir)
	}
	return f.listDir(dir)
}

// ListR lists the objects and directories of the Fs starting
// from dir recursively into out.
//
// dir should be "" to start from the root, and should not
// have trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
//
// It should call callback for each tranche of entries read.
// These need not be returned in any particular order.  If
// callback returns an error then the listing will stop
// immediately.
//
// Don't implement this unless you have a more efficient way
// of listing recursively that doing a directory traversal.
func (f *Fs) ListR(dir string, callback fs.ListRCallback) (err error) {
	if f.container == "" {
		return errors.New("container needed for recursive list")
	}
	list := walk.NewListRHelper(callback)
	err = f.list(dir, true, func(entry fs.DirEntry) error {
		return list.Add(entry)
	})
	if err != nil {
		return err
	}
	// container must be present if listing succeeded
	f.markContainerOK()
	return list.Flush()
}

// About gets quota information
func (f *Fs) About() (*fs.Usage, error) {
	containers, err := f.c.ContainersAll(nil)
	if err != nil {
		return nil, errors.Wrap(err, "container listing failed")
	}
	var total, objects int64
	for _, c := range containers {
		total += c.Bytes
		objects += c.Count
	}
	usage := &fs.Usage{
		Used:    fs.NewUsageValue(total),   // bytes in use
		Objects: fs.NewUsageValue(objects), // objects in use
	}
	return usage, nil
}

// Put the object into the container
//
// Copy the reader in to the new object which is returned
//
// The new object may have been created if an error is returned
func (f *Fs) Put(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Temporary Object under construction
	fs := &Object{
		fs:      f,
		remote:  src.Remote(),
		headers: swift.Headers{}, // Empty object headers to stop readMetaData being called
	}
	return fs, fs.Update(in, src, options...)
}

// PutStream uploads to the remote path with the modTime given of indeterminate size
func (f *Fs) PutStream(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(in, src, options...)
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(dir string) error {
	f.containerOKMu.Lock()
	defer f.containerOKMu.Unlock()
	if f.containerOK {
		return nil
	}
	// if we are at the root, then it is OK
	if f.container == "" {
		return nil
	}
	// Check to see if container exists first
	var err error = swift.ContainerNotFound
	if !f.noCheckContainer {
		_, _, err = f.c.Container(f.container)
	}
	if err == swift.ContainerNotFound {
		headers := swift.Headers{}
		if f.opt.StoragePolicy != "" {
			headers["X-Storage-Policy"] = f.opt.StoragePolicy
		}
		err = f.c.ContainerCreate(f.container, headers)
	}
	if err == nil {
		f.containerOK = true
	}
	return err
}

// Rmdir deletes the container if the fs is at the root
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(dir string) error {
	f.containerOKMu.Lock()
	defer f.containerOKMu.Unlock()
	if f.root != "" || dir != "" {
		return nil
	}
	err := f.c.ContainerDelete(f.container)
	if err == nil {
		f.containerOK = false
	}
	return err
}

// Precision of the remote
func (f *Fs) Precision() time.Duration {
	return time.Nanosecond
}

// Purge deletes all the files and directories
//
// Implemented here so we can make sure we delete directory markers
func (f *Fs) Purge() error {
	// Delete all the files including the directory markers
	toBeDeleted := make(chan fs.Object, fs.Config.Transfers)
	delErr := make(chan error, 1)
	go func() {
		delErr <- operations.DeleteFiles(toBeDeleted)
	}()
	err := f.list("", true, func(entry fs.DirEntry) error {
		if o, ok := entry.(*Object); ok {
			toBeDeleted <- o
		}
		return nil
	})
	close(toBeDeleted)
	delError := <-delErr
	if err == nil {
		err = delError
	}
	if err != nil {
		return err
	}
	return f.Rmdir("")
}

// Copy src to this remote using server side copy operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(src fs.Object, remote string) (fs.Object, error) {
	err := f.Mkdir("")
	if err != nil {
		return nil, err
	}
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	srcFs := srcObj.fs
	_, err = f.c.ObjectCopy(srcFs.container, srcFs.root+srcObj.remote, f.container, f.root+remote, nil)
	if err != nil {
		return nil, err
	}
	return f.NewObject(remote)
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Hash returns the Md5sum of an object returning a lowercase hex string
func (o *Object) Hash(t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	isDynamicLargeObject, err := o.isDynamicLargeObject()
	if err != nil {
		return "", err
	}
	isStaticLargeObject, err := o.isStaticLargeObject()
	if err != nil {
		return "", err
	}
	if isDynamicLargeObject || isStaticLargeObject {
		fs.Debugf(o, "Returning empty Md5sum for swift large object")
		return "", nil
	}
	return strings.ToLower(o.info.Hash), nil
}

// hasHeader checks for the header passed in returning false if the
// object isn't found.
func (o *Object) hasHeader(header string) (bool, error) {
	err := o.readMetaData()
	if err != nil {
		if err == fs.ErrorObjectNotFound {
			return false, nil
		}
		return false, err
	}
	_, isDynamicLargeObject := o.headers[header]
	return isDynamicLargeObject, nil
}

// isDynamicLargeObject checks for X-Object-Manifest header
func (o *Object) isDynamicLargeObject() (bool, error) {
	return o.hasHeader("X-Object-Manifest")
}

// isStaticLargeObjectFile checks for the X-Static-Large-Object header
func (o *Object) isStaticLargeObject() (bool, error) {
	return o.hasHeader("X-Static-Large-Object")
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.info.Bytes
}

// readMetaData gets the metadata if it hasn't already been fetched
//
// it also sets the info
//
// it returns fs.ErrorObjectNotFound if the object isn't found
func (o *Object) readMetaData() (err error) {
	if o.headers != nil {
		return nil
	}
	info, h, err := o.fs.c.Object(o.fs.container, o.fs.root+o.remote)
	if err != nil {
		if err == swift.ObjectNotFound {
			return fs.ErrorObjectNotFound
		}
		return err
	}
	o.info = info
	o.headers = h
	return nil
}

// ModTime returns the modification time of the object
//
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime() time.Time {
	if fs.Config.UseServerModTime {
		return o.info.LastModified
	}
	err := o.readMetaData()
	if err != nil {
		fs.Debugf(o, "Failed to read metadata: %s", err)
		return o.info.LastModified
	}
	modTime, err := o.headers.ObjectMetadata().GetModTime()
	if err != nil {
		// fs.Logf(o, "Failed to read mtime from object: %v", err)
		return o.info.LastModified
	}
	return modTime
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(modTime time.Time) error {
	err := o.readMetaData()
	if err != nil {
		return err
	}
	meta := o.headers.ObjectMetadata()
	meta.SetModTime(modTime)
	newHeaders := meta.ObjectHeaders()
	for k, v := range newHeaders {
		o.headers[k] = v
	}
	// Include any other metadata from request
	for k, v := range o.headers {
		if strings.HasPrefix(k, "X-Object-") {
			newHeaders[k] = v
		}
	}
	return o.fs.c.ObjectUpdate(o.fs.container, o.fs.root+o.remote, newHeaders)
}

// Storable returns if this object is storable
//
// It compares the Content-Type to directoryMarkerContentType - that
// makes it a directory marker which is not storable.
func (o *Object) Storable() bool {
	return o.info.ContentType != directoryMarkerContentType
}

// Open an object for read
func (o *Object) Open(options ...fs.OpenOption) (in io.ReadCloser, err error) {
	headers := fs.OpenOptionHeaders(options)
	_, isRanging := headers["Range"]
	in, _, err = o.fs.c.ObjectOpen(o.fs.container, o.fs.root+o.remote, !isRanging, headers)
	return
}

// min returns the smallest of x, y
func min(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}

// removeSegments removes any old segments from o
//
// if except is passed in then segments with that prefix won't be deleted
func (o *Object) removeSegments(except string) error {
	segmentsRoot := o.fs.root + o.remote + "/"
	err := o.fs.listContainerRoot(o.fs.segmentsContainer, segmentsRoot, "", true, func(remote string, object *swift.Object, isDirectory bool) error {
		if isDirectory {
			return nil
		}
		if except != "" && strings.HasPrefix(remote, except) {
			// fs.Debugf(o, "Ignoring current segment file %q in container %q", segmentsRoot+remote, o.fs.segmentsContainer)
			return nil
		}
		segmentPath := segmentsRoot + remote
		fs.Debugf(o, "Removing segment file %q in container %q", segmentPath, o.fs.segmentsContainer)
		return o.fs.c.ObjectDelete(o.fs.segmentsContainer, segmentPath)
	})
	if err != nil {
		return err
	}
	// remove the segments container if empty, ignore errors
	err = o.fs.c.ContainerDelete(o.fs.segmentsContainer)
	if err == nil {
		fs.Debugf(o, "Removed empty container %q", o.fs.segmentsContainer)
	}
	return nil
}

// urlEncode encodes a string so that it is a valid URL
//
// We don't use any of Go's standard methods as we need `/` not
// encoded but we need '&' encoded.
func urlEncode(str string) string {
	var buf bytes.Buffer
	for i := 0; i < len(str); i++ {
		c := str[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '/' || c == '.' {
			_ = buf.WriteByte(c)
		} else {
			_, _ = buf.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return buf.String()
}

// updateChunks updates the existing object using chunks to a separate
// container.  It returns a string which prefixes current segments.
func (o *Object) updateChunks(in0 io.Reader, headers swift.Headers, size int64, contentType string) (string, error) {
	// Create the segmentsContainer if it doesn't exist
	var err error
	_, _, err = o.fs.c.Container(o.fs.segmentsContainer)
	if err == swift.ContainerNotFound {
		headers := swift.Headers{}
		if o.fs.opt.StoragePolicy != "" {
			headers["X-Storage-Policy"] = o.fs.opt.StoragePolicy
		}
		err = o.fs.c.ContainerCreate(o.fs.segmentsContainer, headers)
	}
	if err != nil {
		return "", err
	}
	// Upload the chunks
	left := size
	i := 0
	uniquePrefix := fmt.Sprintf("%s/%d", swift.TimeToFloatString(time.Now()), size)
	segmentsPath := fmt.Sprintf("%s%s/%s", o.fs.root, o.remote, uniquePrefix)
	in := bufio.NewReader(in0)
	for {
		// can we read at least one byte?
		if _, err := in.Peek(1); err != nil {
			if left > 0 {
				return "", err // read less than expected
			}
			fs.Debugf(o, "Uploading segments into %q seems done (%v)", o.fs.segmentsContainer, err)
			break
		}
		n := int64(o.fs.opt.ChunkSize)
		if size != -1 {
			n = min(left, n)
			headers["Content-Length"] = strconv.FormatInt(n, 10) // set Content-Length as we know it
			left -= n
		}
		segmentReader := io.LimitReader(in, n)
		segmentPath := fmt.Sprintf("%s/%08d", segmentsPath, i)
		fs.Debugf(o, "Uploading segment file %q into %q", segmentPath, o.fs.segmentsContainer)
		_, err := o.fs.c.ObjectPut(o.fs.segmentsContainer, segmentPath, segmentReader, true, "", "", headers)
		if err != nil {
			return "", err
		}
		i++
	}
	// Upload the manifest
	headers["X-Object-Manifest"] = urlEncode(fmt.Sprintf("%s/%s", o.fs.segmentsContainer, segmentsPath))
	headers["Content-Length"] = "0" // set Content-Length as we know it
	emptyReader := bytes.NewReader(nil)
	manifestName := o.fs.root + o.remote
	_, err = o.fs.c.ObjectPut(o.fs.container, manifestName, emptyReader, true, "", contentType, headers)
	return uniquePrefix + "/", err
}

// Update the object with the contents of the io.Reader, modTime and size
//
// The new object may have been created if an error is returned
func (o *Object) Update(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if o.fs.container == "" {
		return fserrors.FatalError(errors.New("container name needed in remote"))
	}
	err := o.fs.Mkdir("")
	if err != nil {
		return err
	}
	size := src.Size()
	modTime := src.ModTime()

	// Note whether this is a dynamic large object before starting
	isDynamicLargeObject, err := o.isDynamicLargeObject()
	if err != nil {
		return err
	}

	// Set the mtime
	m := swift.Metadata{}
	m.SetModTime(modTime)
	contentType := fs.MimeType(src)
	headers := m.ObjectHeaders()
	uniquePrefix := ""
	if size > int64(o.fs.opt.ChunkSize) || size == -1 {
		uniquePrefix, err = o.updateChunks(in, headers, size, contentType)
		if err != nil {
			return err
		}
	} else {
		headers["Content-Length"] = strconv.FormatInt(size, 10) // set Content-Length as we know it
		_, err := o.fs.c.ObjectPut(o.fs.container, o.fs.root+o.remote, in, true, "", contentType, headers)
		if err != nil {
			return err
		}
	}

	// If file was a dynamic large object then remove old/all segments
	if isDynamicLargeObject {
		err = o.removeSegments(uniquePrefix)
		if err != nil {
			fs.Logf(o, "Failed to remove old segments - carrying on with upload: %v", err)
		}
	}

	// Read the metadata from the newly created object
	o.headers = nil // wipe old metadata
	return o.readMetaData()
}

// Remove an object
func (o *Object) Remove() error {
	isDynamicLargeObject, err := o.isDynamicLargeObject()
	if err != nil {
		return err
	}
	// Remove file/manifest first
	err = o.fs.c.ObjectDelete(o.fs.container, o.fs.root+o.remote)
	if err != nil {
		return err
	}
	// ...then segments if required
	if isDynamicLargeObject {
		err = o.removeSegments("")
		if err != nil {
			return err
		}
	}
	return nil
}

// MimeType of an Object if known, "" otherwise
func (o *Object) MimeType() string {
	return o.info.ContentType
}

// Check the interfaces are satisfied
var (
	_ fs.Fs          = &Fs{}
	_ fs.Purger      = &Fs{}
	_ fs.PutStreamer = &Fs{}
	_ fs.Copier      = &Fs{}
	_ fs.ListRer     = &Fs{}
	_ fs.Object      = &Object{}
	_ fs.MimeTyper   = &Object{}
)
