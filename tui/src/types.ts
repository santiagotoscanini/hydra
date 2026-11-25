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
	parent: string;
	action?: "merge" | "fast-forward";
	note: string;
	hand?: boolean;
	conflicts?: string[];
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
	pending?: boolean;
	plannedAt: string;
	gitAt: string;
	error?: string;
}

export interface Snapshot {
	version: number;
	repo: string;
	current: string;
	baseRef: string;
	at: string;
	main?: MainState;
	worktrees: WorktreeState[];
	error?: string;
}
