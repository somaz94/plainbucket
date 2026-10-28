# Storage Layout

plainbucket keeps the data a client uploads as ordinary files and everything else in one system directory beside the buckets.

<br/>

## Buckets and Objects

- A bucket is a directory directly under the root. Any existing directory whose name is a valid S3 bucket name is a bucket, so pointing plainbucket at a directory that already holds data needs no import step. Directories with other names are ignored.
- Object `<key>` in bucket `<b>` is the file `<root>/<b>/<key>`, each `/` in the key being a directory level. The file holds exactly the uploaded bytes.
- Files are written `0644` and directories `0755`, so a server running as another user, such as a static file server, can read them.
- An upload is written to a staging file first and renamed into place once it is complete and verified, so readers never see a partial object. The data, its metadata and the directories they land in are flushed to disk before the request is answered, so an acknowledged write survives a power loss; on macOS this costs small writes noticeably more than on Linux. A directory flush that fails once the file is in place does not fail the write: the server counts it in `plainbucket_sync_errors_total` and logs it at most once a minute, and says nothing when the filesystem does not support flushing directories, as some network filesystems do not. Two writes that create the same new directories at once rely on the filesystem committing those entries in order, as ext4, XFS and ZFS do. Deletes are not flushed: one acknowledged just before a power loss may come back, and replication or expiry handles it as any other object.

<br/>

## The System Directory

`<root>/.plainbucket` holds the metadata of each object, staging files, unfinished multipart uploads, the [trash](configuration.md#trash), bucket creation times and tags, the result of the last [integrity check](configuration.md#integrity-checks) (`scrub.json`), and a `lock` file the running server holds so that a second server or `plainbucket fsck` cannot work on the same root at once. Bucket names cannot start with a dot, so it can never collide with a bucket. Publish the root with a server that hides dot files, or not at all.

<br/>

## Metadata and Files Changed Outside the Server

Each object's ETag, content type, user metadata, stored headers, tags and checksums, and for a multipart object the sizes of its parts, sit in a metadata file under the system directory. That file also records the object's size and modification time, and it is trusted only while both still match the data file. The metadata file's own modification time is the object's last read, used by [expiry and size limits](configuration.md#expiry-and-size-limits).

When a file was changed, or added, without the server:

- `GetObject` and `HeadObject` recompute the MD5 ETag from the file, keep the previous content type, user metadata and tags, and save the result.
- Listings do not read file contents. They report a placeholder ETag ending in `-1`, the multipart ETag form, so clients do not mistake it for the file's MD5. The next `GetObject` or `HeadObject` replaces it with the real one.
- A file with no metadata at all gets a content type guessed from its extension.

<br/>

## Directory Markers

A zero-byte object whose key ends in `/` (`photos/2026/`) is a directory marker, as the S3 console creates for folders. plainbucket creates the directory and records the marker in the system directory, so it is listed and kept even after objects below it come and go. A non-empty body for such a key is rejected.

A directory without a marker, whether made outside the server or created for the objects below it, is listed and read as a marker only while it is empty. Once it holds anything, `GET` and `HEAD` of `<dir>/` answer `NoSuchKey`, as on S3.

<br/>

## Limits of the Mapping

S3 keys are arbitrary strings; file paths are not. The following differ from S3:

- **A key cannot also be a prefix.** With object `a` stored as a file, `a/b` would need `a` to be a directory, and the reverse. The later write fails with `409 KeyConflict`.
- **Keys must be valid relative paths.** Empty segments (`a//b`), `.` and `..` segments and NUL bytes are rejected with `400 InvalidArgument`. A segment longer than 255 bytes, or a key longer than 1024 bytes, is rejected with `400 KeyTooLongError`.
- **Case-insensitive filesystems fold keys.** On such a filesystem, macOS's default included, `Photo.jpg` and `photo.jpg` are one file. The server logs a warning at startup when it detects this.
- **Unicode normalization follows the filesystem.** Keys are stored byte for byte. On a normalization-insensitive filesystem (APFS), the NFC and NFD spellings of `café` name one file; on ext4 and XFS they are two objects. A listing returns the name as stored on disk, which on HFS+ is the NFD form even for a key written in NFC.
- **Empty prefixes disappear.** Deleting the last object under `a/b/` removes the empty directories `a/b` and `a`, stopping at a directory created as an explicit marker.
- **A listing reads each directory it passes whole.** Every page of a listing reads all the names in the directories it walks, though it stats only the entries it reaches, about as many as it returns. A folder holding hundreds of thousands of objects therefore costs more per page than the same objects spread over subfolders.

<br/>

## Symlinks

Every path is resolved inside the root directory, so a symlink placed in a bucket cannot expose or overwrite files elsewhere.

- A relative symlink to a file inside the root is an object.
- A symlink whose target is absolute, or leaves the root, is ignored by listings and reads as missing.
- Symlinks to directories are not listed, so a link cannot make a listing loop. A key that passes through one still resolves while the target stays inside the root.

<br/>

## Listing Order

S3 lists keys in byte order of the whole key. A plain directory walk does not produce that order: `a-c` sorts before `a/b` because `-` is below `/`, yet a walk reaches the contents of directory `a` before the file `a-c`. The listing code sorts each directory's entries with a trailing `/` on directory names, which yields S3 order without reading the whole bucket.
