// Package onepassword reads secrets from 1Password through the 1Password Go
// SDK (github.com/1password/onepassword-sdk-go), authenticated as a service
// account, so nothing in workingman needs the `op` CLI, `op signin` or a
// biometric unlock.
//
// Role: daemon
//
// Its one caller is cmd/op-read, which start.sh runs once per start to pipe
// the commit-signing SSH key into the daemon's private ssh-agent
// (ORCH_SSH_KEY_REF, see docs/ssh-agent.md).
//
// # Service account token
//
// [ResolveToken] looks for the token in this order:
//
//  1. the OP_SERVICE_ACCOUNT_TOKEN environment variable, when set and not
//     blank — this is how tests, CI and sandboxes inject it;
//  2. otherwise, on macOS, the keychain item with service
//     "workingman:service_account" and account "jim.clark", read with
//     `security find-generic-password -a jim.clark -s workingman:service_account -w`.
//
// The service (-s), account (-a) and an optional keychain to search (the
// trailing argument to security; by default the user's keychain search list,
// which includes the login keychain) can be overridden with
// OP_SA_KEYCHAIN_SERVICE, OP_SA_KEYCHAIN_ACCOUNT and OP_SA_KEYCHAIN.
//
// If neither yields a token, ResolveToken fails with an error saying how to
// provide one. Surrounding whitespace (the newline `security -w` prints) is
// trimmed. The token is never passed on a command line, logged, or included
// in an error.
//
// To seed the keychain item (security prompts for the token, keeping it out
// of argv and shell history; -U replaces an existing item):
//
//	security add-generic-password -U -a jim.clark -s workingman:service_account -w
//
// The trailing -w must stay last for security to prompt, so it cannot be
// followed by a keychain argument: to store the item in a separate keychain
// file instead of the login keychain (e.g. jim.clark.keychain-db), add it
// with Keychain Access, or make that keychain the default for the command
// (`security default-keychain -s jim.clark.keychain-db`, then restore it with
// `security default-keychain -s login.keychain-db`), and set
// OP_SA_KEYCHAIN=jim.clark.keychain-db.
//
// This is the same lookup standalone-codemode's internal/opsdk does, so one
// keychain item serves both.
//
// # Reading a secret
//
// [Read] resolves an op:// secret reference with the SDK's
// Secrets().Resolve. The SDK honours the `?ssh-format=openssh` query on SSH
// key fields; as a fallback, when a reference asks for that format but the
// value comes back as a PEM private key in another format (PKCS#1, PKCS#8,
// SEC 1), Read converts it to OpenSSH with golang.org/x/crypto/ssh. A service
// account sees only the vaults it was granted — never a "Personal" vault — so
// the referenced item must live in a shared vault the account can read.
//
// # Key types
//
//   - [TokenSource] — env + keychain token resolution, with injectable
//     Getenv and [KeychainLookup] seams for tests.
//   - [KeychainItem] — the service/account/keychain of the token's item.
//   - [Resolver] — the one SDK call Read needs; *onepassword.Client's
//     Secrets() satisfies it, tests pass a fake.
//
// # Collaborators
//
// No internal imports. Used only by cmd/op-read.
package onepassword
