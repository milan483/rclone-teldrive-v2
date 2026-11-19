---
title: "TelDrive"
description: "Rclone docs for TelDrive"
versionIntroduced: "v1.69"
---

# TelDrive

TelDrive is a self-hosted file storage solution that uses Telegram
channels as the underlying storage backend. Files are split into chunks,
optionally encrypted, and uploaded as messages in a designated Telegram
channel or group. The TelDrive API server indexes and manages the file
metadata, providing a file-system-like interface over Telegram's storage.

Paths are specified as `remote:path`.

To use TelDrive, you need:
- A running TelDrive server (self-hosted)
- An API key for authentication
- A Telegram channel ID to use as storage

## Configuration

Here is an example of making a remote for TelDrive.

First run:

```console
rclone config
```

This will guide you through an interactive setup process:

```text
No remotes found, make a new one?
n) New remote
d) Delete remote
q) Quit config
n/d/q> n

Enter name for new remote.
name> remote

Option Storage.
Type of storage to configure.
Choose a number from below, or type in your own value.
XX / TelDrive
   \ (teldrive)
Storage> XX

Option api_host.
URL of the TelDrive API server.
Enter a value.
api_host> https://teldrive.example.com

Option api_key.
API key for authentication.
Enter a value.
api_key> YOUR_API_KEY

Option channel_id.
Telegram channel ID for storage.
Enter a value.
channel_id> -1001234567890

Configuration complete.
Options:
- type: teldrive
- api_host: https://teldrive.example.com
- api_key: XXXXXXXXXXX
- channel_id: -1001234567890
Keep this "remote" remote?
y) Yes this is OK (default)
e) Edit this remote
d) Delete this remote
y/e/d> y
```

## Encryption and mounts

TelDrive and rclone provide two different encryption layers:

* `encrypt_files` (the `--teldrive-encrypt-files` option) is TelDrive's
  native **server-side encryption**. The TelDrive server encrypts files before
  storing them in Telegram and manages the encryption keys. This protects
  stored data from the Telegram storage layer, but the TelDrive server remains
  part of the trust boundary and can decrypt the files.
* `crypt` is rclone's **client-side encryption**. Rclone encrypts file data and
  names before sending them to the underlying remote, using the crypt
  password and salt. The TelDrive server and Telegram only receive the
  encrypted rclone data.

If confidentiality from the TelDrive server is required, put an rclone crypt
remote on top of the TelDrive remote. TelDrive's native encryption can still
be enabled as an additional at-rest layer, but it does not replace crypt for
end-to-end confidentiality.

### Configure an encrypted TelDrive remote

The following sequence creates a TelDrive remote named `teldrive`, then a
crypt remote named `teldrive-crypt` whose underlying remote is `teldrive:`:

```console
$ rclone config
# New remote: teldrive
# Storage: teldrive
# Set api_host, api_key, channel_id, and (optionally) encrypt_files.
# Then choose "Yes this is OK".

$ rclone config
# New remote: teldrive-crypt
# Storage: crypt
# Remote to encrypt: teldrive:
# Set and record the crypt password and salt.
```

The equivalent relationship in `rclone.conf` is:

```ini
[teldrive]
type = teldrive
api_host = https://teldrive.example.com
api_key = YOUR_API_KEY
channel_id = -1001234567890
encrypt_files = true

[teldrive-crypt]
type = crypt
remote = teldrive:
```

Use `rclone config` to create the crypt password and salt; do not put literal
passwords in scripts or commit this file. The rclone configuration contains
the credentials and key material needed to access both remotes and should be
backed up securely with restricted permissions.

### Mount examples

Mount the plain TelDrive remote when you intentionally want the files exposed
by TelDrive directly:

```console
$ mkdir -p ~/mnt/teldrive
$ rclone mount teldrive: ~/mnt/teldrive --vfs-cache-mode writes
```

Mount the crypt remote for the recommended end-to-end setup. The mount point
shows decrypted filenames and file contents to local applications, while the
remote stores the crypt-encrypted representation:

```console
$ mkdir -p ~/mnt/teldrive-crypt
$ rclone mount teldrive-crypt: ~/mnt/teldrive-crypt --vfs-cache-mode full
```

For a read-only view, add `--read-only` (to either mount command):

```console
$ rclone mount teldrive-crypt: ~/mnt/teldrive-crypt \
    --read-only --vfs-cache-mode full
```

Choose the cache mode for the workload: `writes` is useful when applications
need write buffering, while `full` caches file data for applications that
expect normal filesystem seeking. Caches contain plaintext for a crypt mount;
protect the cache directory and unmount the filesystem when it is not needed.

Do not lose the crypt password or salt. Without them, rclone cannot decrypt
the data or recover encrypted names. Do not change crypt settings such as the
password, salt, filename encryption, or directory-name encryption after files
have been created unless you are deliberately migrating the data with the
appropriate rclone procedure; otherwise existing files may become
unreadable. TelDrive native encryption and rclone crypt are not interchangeable
keys or formats.

The mount point is a plaintext exposure point: local users, processes, logs,
swap, and cache storage may be able to access decrypted data. Secure the host,
mount directory, and cache, and remember that `--read-only` prevents writes
through the mount but does not make already readable data confidential.

<!-- autogenerated options start - DO NOT EDIT - instead edit fs.RegInfo in backend/teldrive/teldrive.go and run make backenddocs to verify --> <!-- markdownlint-disable-line line-length -->
### Standard options

Here are the Standard options specific to teldrive (Tel Drive).

#### --teldrive-api-key

API key for authentication with the TelDrive server.

This is the API token used to authenticate requests. Obtain it from
your TelDrive dashboard or server admin.


Properties:

- Config:      api_key
- Env Var:     RCLONE_TELDRIVE_API_KEY
- Type:        string
- Required:    false

#### --teldrive-api-host

URL of the TelDrive API server.

The base URL for the TelDrive API endpoint (e.g. https://teldrive.example.com).
This is required for all operations.


Properties:

- Config:      api_host
- Env Var:     RCLONE_TELDRIVE_API_HOST
- Type:        string
- Required:    false

#### --teldrive-chunk-size

Upload chunk size.

Files larger than this will be uploaded in multiple chunks. The chunk size
is automatically aligned to the nearest 16 MiB multiple (the BLAKE3 tree
hash block size) for optimal hashing performance.

Larger chunk sizes reduce the number of API calls but use more memory
during uploads (one chunk is buffered per concurrent upload stream).
Minimum: 64 MiB, Maximum: 2000 MiB.


Properties:

- Config:      chunk_size
- Env Var:     RCLONE_TELDRIVE_CHUNK_SIZE
- Type:        SizeSuffix
- Default:     512Mi

#### --teldrive-link-password

Password to set on created public links.

When set, all public links created via the link command will require
this password to access. If not set, links will be publicly accessible.


Properties:

- Config:      link_password
- Env Var:     RCLONE_TELDRIVE_LINK_PASSWORD
- Type:        string
- Required:    false

#### --teldrive-page-size

Number of items to return per page when listing files.

Controls the page size for directory listing API calls. Larger values
reduce the number of API requests needed for directories with many files,
but may increase response time and memory usage for each request.


Properties:

- Config:      page_size
- Env Var:     RCLONE_TELDRIVE_PAGE_SIZE
- Type:        int
- Default:     500

#### --teldrive-channel-id

Telegram channel ID where files will be stored.

The ID of the Telegram channel or group used as storage backend by
TelDrive. This should be the numeric ID of the channel. If the ID is
negative (e.g. -1001234567890), the -100 prefix will be automatically
stripped as required by the TelDrive API.


Properties:

- Config:      channel_id
- Env Var:     RCLONE_TELDRIVE_CHANNEL_ID
- Type:        string
- Required:    false

#### --teldrive-upload-host

Optional alternative URL for upload API calls.

If set, upload chunk requests will be sent to this host instead of
the main api_host. Useful for load-balancing or directing upload
traffic to a different server or CDN endpoint.


Properties:

- Config:      upload_host
- Env Var:     RCLONE_TELDRIVE_UPLOAD_HOST
- Type:        string
- Required:    false

#### --teldrive-encrypt-files

Enable native TelDrive encryption for stored files.

When enabled, files will be encrypted at rest using TelDrive's
built-in encryption before being sent to Telegram. The encryption
keys are managed by the TelDrive server.


Properties:

- Config:      encrypt_files
- Env Var:     RCLONE_TELDRIVE_ENCRYPT_FILES
- Type:        bool
- Default:     false

#### --teldrive-hash-enabled

Enable BLAKE3 tree hashing for file integrity verification.

Files are split into 16 MiB blocks, each hashed with BLAKE3, then
the block hashes are combined into a final tree hash. This allows
rclone to verify upload integrity and detect modifications.

Disable only if the server does not support this hash type.


Properties:

- Config:      hash_enabled
- Env Var:     RCLONE_TELDRIVE_HASH_ENABLED
- Type:        bool
- Default:     true

### Advanced options

Here are the Advanced options specific to teldrive (Tel Drive).

#### --teldrive-upload-concurrency

Number of chunks to upload in parallel per file.

Higher concurrency can speed up large file uploads at the cost of
more memory usage and potential rate-limiting. Each concurrent upload
buffers one chunk in memory.


Properties:

- Config:      upload_concurrency
- Env Var:     RCLONE_TELDRIVE_UPLOAD_CONCURRENCY
- Type:        int
- Default:     4

#### --teldrive-threaded-streams

Use threaded message streams for uploads.

When enabled, uploads will use Telegram's threaded message replies
to organize file parts, which can improve performance in channels
with many concurrent uploads.


Properties:

- Config:      threaded_streams
- Env Var:     RCLONE_TELDRIVE_THREADED_STREAMS
- Type:        bool
- Default:     false

#### --teldrive-encoding

The encoding for the backend.

See the [encoding section in the overview](/overview/#encoding) for more info.

Properties:

- Config:      encoding
- Env Var:     RCLONE_TELDRIVE_ENCODING
- Type:        Encoding
- Default:     Slash,Del,Ctl,InvalidUtf8,Dot

#### --teldrive-description

Description of the remote.

Properties:

- Config:      description
- Env Var:     RCLONE_TELDRIVE_DESCRIPTION
- Type:        string
- Required:    false

<!-- autogenerated options stop -->

## Limitations

Invalid UTF-8 bytes will be replaced as they can't be used in JSON
strings. Filenames containing certain special characters may be encoded
as configured by the `--teldrive-encoding` option.
