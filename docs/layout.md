# Storage Layout

plainbucket keeps the data a client uploads as ordinary files and everything else in one system directory beside the buckets.

<br/>

## Buckets and Objects

- A bucket is a directory directly under the root. Any existing directory whose name is a valid S3 bucket name is a bucket, so pointing plainbucket at a directory that already holds data needs no import step. Directories with other names are ignored.
- Object `<key>` in bucket `<b>` is the file `<root>/<b>/<key>`, each `/` in the key being a directory level. The file holds exactly the uploaded bytes.
- Files are written `0644` and directories `0755`, so a server running as another user, such as a static file server, can read them.
- An upload is written to a staging file first and renamed into place once it is complete and verified, so readers never see a partial object.

<br/>

## The System Directory

`<root>/.plainbucket` holds the metadata of each object, staging files, unfinished multipart uploads and bucket creation times. Bucket names cannot start with a dot, so it can never collide with a bucket. Publish the root with a server that hides dot files, or not at all.

<br/>

## Metadata and Files Changed Outside the Server

Each object's ETag, content type, user metadata, stored headers and checksums sit in a metadata file under the system directory. That file also records the object's size and modification time, and it is trusted only while both still match the data file.

When a file was changed, or added, without the server:

- `GetObject` and `HeadObject` recompute the MD5 ETag from the file, keep the previous content type and user metadata, and save the result.
- Listings do not read file contents. They report a placeholder ETag ending in `-1`, the multipart ETag form, so clients do not mistake it for the file's MD5. The next `GetObject` or `HeadObject` replaces it with the real one.
- A file with no metadata at all gets a content type guessed from its extension.

<br/>

## Directory Markers

A zero-byte object whose key ends in `/` (`photos/2026/`) is a directory marker, as the S3 console creates for folders. plainbucket creates the directory and records the marker in the system directory, so it is listed and kept even after objects below it come and go. A non-empty body for such a key is rejected.

A directory that exists without a marker, because it was made outside the server, is listed as a marker only while it is empty.

<br/>

## Limits of the Mapping

S3 keys are arbitrary strings; file paths are not. The following differ from S3:

- **A key cannot also be a prefix.** With object `a` stored as a file, `a/b` would need `a` to be a directory, and the reverse. The later write fails with `409 KeyConflict`.
- **Keys must be valid relative paths.** Empty segments (`a//b`), `.` and `..` segments and NUL bytes are rejected with `400 InvalidArgument`. A segment longer than 255 bytes, or a key longer than 1024 bytes, is rejected with `400 KeyTooLongError`.
- **Case-insensitive filesystems fold keys.** On such a filesystem, macOS's default included, `Photo.jpg` and `photo.jpg` are one file. The server logs a warning at startup when it detects this.
- **Unicode normalization follows the filesystem.** Keys are stored byte for byte. On a normalization-insensitive filesystem (APFS), the NFC and NFD spellings of `café` name one file; on ext4 and XFS they are two objects. A listing returns the name as stored on disk, which on HFS+ is the NFD form even for a key written in NFC.
- **Empty prefixes disappear.** Deleting the last object under `a/b/` removes the empty directories `a/b` and `a`, stopping at a directory created as an explicit marker.

<br/>

## Symlinks

Every path is resolved inside the root directory, so a symlink placed in a bucket cannot expose or overwrite files elsewhere.

- A relative symlink to a file inside the root is an object.
- A symlink whose target is absolute, or leaves the root, is ignored by listings and reads as missing.
- Symlinks to directories are not listed, so a link cannot make a listing loop. A key that passes through one still resolves while the target stays inside the root.

<br/>

## Listing Order

S3 lists keys in byte order of the whole key. A plain directory walk does not produce that order: `a-c` sorts before `a/b` because `-` is below `/`, yet a walk reaches the contents of directory `a` before the file `a-c`. The listing code sorts each directory's entries with a trailing `/` on directory names, which yields S3 order without reading the whole bucket.
