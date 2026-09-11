// Package isolate owns fixed local provider execution without vault ownership or database I/O.
//
// The native Linux runner admits only --protocol docbank-trafilatura/v2 and
// --protocol docbank-pymupdf/v1 with the six fixed locale and Python environment
// entries. It verifies and seals the executable bytes, then seals the final
// executable argv and environment with a random authenticated launch token.
// It requires user, network, PID and mount namespaces, private read-only /proc,
// no_new_privs, and seccomp denial of sockets and io_uring, including ABI checks.
// Parent death, cancellation and output overflow terminate the namespace init
// and its descendants. Deadlines and output bounds limit each operation.
//
// NewNativeRunner refuses unsupported platforms. Linux launch also refuses
// unavailable kernel controls. Injected IsolatedRunner implementations are
// trusted deployment components; their attestations require independent audit.
// The runner imposes no CPU or address-space quota and no general confinement
// of host filesystem access. Parser code retains daemon-user filesystem privileges.
//
// Native identity v3 includes both fixed protocols and sealed argv/environment.
// Provider descriptors that include the native runner identity change with it.
package isolate
