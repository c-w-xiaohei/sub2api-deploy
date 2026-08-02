export type Slot = "blue" | "green";

export interface DeployState {
  activeSlot: Slot;
  previousSlot?: Slot;
  activeImage: string;
  previousImage?: string;
  postgresMode?: "docker" | "neon";
  redisMode?: "docker" | "upstash";
}

export function nextSlot(state: DeployState | undefined): Slot {
  return state?.activeSlot === "blue" ? "green" : "blue";
}

export function transitionState(state: DeployState, activeSlot: Slot, activeImage: string): DeployState {
  return {
    activeSlot,
    previousSlot: state.activeSlot,
    activeImage,
    previousImage: state.activeImage,
    ...(state.postgresMode ? { postgresMode: state.postgresMode } : {}),
    ...(state.redisMode ? { redisMode: state.redisMode } : {}),
  };
}
