// Fixture: a workspaces-layout frontend whose stores live in `src/store/`
// (singular), the directory an adopted app keeps. The rule must reach it:
// before, only `src/stores/` was scanned, so such an app was never linted.

import { create } from "zustand";
import { WorktreeStatus } from "../gen/reliant/v1/worktree_pb";

type WorktreeState = {
  status: WorktreeStatus;
};

export const useWorktreeStore = create<WorktreeState>(() => ({
  status: WorktreeStatus.UNSPECIFIED,
}));
