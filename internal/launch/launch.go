// Package launch starts background helper processes that must outlive the
// process which spawns them.
//
// This exists because of how Chrome drives a native-messaging host: each
// chrome.runtime.sendNativeMessage call starts the host, reads one response and
// then tears the host down. On Windows the host may also live inside a job
// object that kills its children, so a plain child process would be killed
// mid-download. Detached breaks out of both.
package launch
