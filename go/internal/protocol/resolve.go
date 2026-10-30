package protocol

// TypeResolvePath is an app → agent request on the control stream: which
// file-provider document is the absolute guest Path (a terminal's file://
// link)? The answer is a [Done] of this type whose Path is relative to
// files.root ("" for the root itself), or an error when the path, symlinks
// resolved, is outside it.
const TypeResolvePath = "resolve-path"
