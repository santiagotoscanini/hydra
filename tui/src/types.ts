// What `hydra status --json` prints (cmd/hydra/snapshot.go). Version 1.

export interface Problem {
	blocks: boolean;
	passing?: boolean;
	text: string;
	fix?: string;
	/** The fix as a hydra command to run in the checkout the problem is about. */
	run?: string;
}

export type Health = "up" | "done" | "starting" | "off" | "failed" | "unknown";

export interface Service {
	name: string;
	/** "here", "main" (main's copy), or "routed" (main's copy through this worktree's port). */
	runs: "here" | "main" | "routed";
	off?: boolean;
	used?: boolean;
	state: string;
	health: Health;
	port?: number;
	url?: string;
	/** Its address by checkout name (x-hydra hosts): http://<worktree>.localhost:PORT. */
	address?: string;
	why?: string;
	memory?: number;
}

export interface Skew {
	service: string;
	why: string;
	files: string[];
}

export interface MainState {
	root: string;
	branch: string;
	commit: string;
	subject: string;
	behind: number;
	dirty: number;
	up: boolean;
	where: string;
	processes: Service[];
	memory: number;
	problems: Problem[];
}

export interface Update {
	/** The branch merged in: the base ref (main), or the one it is stacked on. */
	parent: string;
	/** Commits the parent has that it doesn't. */
	behind?: number;
	action?: "merge" | "fast-forward";
	note: string;
	hand?: boolean;
	conflicts?: string[];
}

/** A branch's pull request, read from GitHub with gh (absent from older
 *  hydras, and when gh can't be asked). */
export interface PR {
	/** absent when only git says it is merged (mergedLocally) */
	number?: number;
	state: "open" | "draft" | "merged" | "closed";
	review?: "approved" | "changes requested" | "review required";
	checks?: "passing" | "failing" | "pending";
	url?: string;
	/** the branch it merges into */
	base?: string;
	/** its commits are in origin's main, merged with a merge commit */
	mergedLocally?: boolean;
}

/** When gh was last asked for the PRs, and why it can't be. */
export interface PRSource {
	at?: string;
	error?: string;
}

export interface WorktreeState {
	root: string;
	dir: string;
	branch: string;
	slot: number;
	up: boolean;
	commit: string;
	subject: string;
	behind: number;
	ahead: number;
	stack?: string[];
	mainBehind?: boolean;
	outOfStep: Skew[];
	services: Service[];
	memory: number;
	problems: Problem[];
	update?: Update;
	pr?: PR;
	/** Its stack isn't running because nobody used it for x-hydra idle_stop. */
	idleStopped?: { at: string; after: string };
	/** More than behind_warn commits behind main, or merging main conflicts. */
	farBehind?: { behind: number; limit: number; conflicts?: string[] };
	pending?: boolean;
	plannedAt: string;
	gitAt: string;
	error?: string;
}

/** A branch of a PR stack that a worktree sits on but no worktree has
 *  checked out: hydra runs that worktree on its code. */
export interface Branch {
	branch: string;
	commit: string;
	subject: string;
	/** Commits main's checkout has that it doesn't. */
	behind: number;
	ahead: number;
	/** The chain, bottom first, its own last. */
	stack: string[];
	/** How hydra knows it is a stack branch. */
	source: "gh-stack" | "ancestry";
	update?: Update;
	pr?: PR;
	pending?: boolean;
	gitAt: string;
}

export interface Snapshot {
	version: number;
	repo: string;
	current: string;
	baseRef: string;
	at: string;
	main?: MainState;
	worktrees: WorktreeState[];
	/** Stack branches with no checkout (absent from older hydras). */
	branches?: Branch[];
	prs?: PRSource;
	error?: string;
}
