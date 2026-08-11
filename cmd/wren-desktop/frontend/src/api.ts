export type WrenContext = { name: string; server: string; org?: string; user?: string; selected: boolean }
export type Project = {
  name: string; repo?: string; defaultHarness?: string; harnessImage?: string;
  defaultModel?: string; runtimeClass?: string; cpu?: string; memory?: string;
  disk?: string; namespace?: string; createdAt?: string
}
export type Run = {
  id: string; project: string; user?: string; phase: string; harness?: string;
  namespace?: string; prUrl?: string; restartCount?: number; createdAt?: string
}
export type Bootstrap = { contexts: WrenContext[]; projects: Project[]; runs: Run[] }
export type RunCreate = {
  project: string; task: string; harness?: string; interactive?: boolean;
  baseRef?: string; cpu?: string; memory?: string; runtime?: string
}
export type LogEvent = { streamId: string; chunk?: string; error?: string; done?: boolean }

type Backend = {
  Load(): Promise<Bootstrap>
  SelectContext(name: string): Promise<Bootstrap>
  SaveContext(context: { name: string; server: string; org?: string; user?: string; token?: string }): Promise<Bootstrap>
  ListRuns(scope: string, project: string, phase: string): Promise<Run[]>
  GetRun(id: string): Promise<Run>
  CreateRun(options: RunCreate): Promise<Run>
  StopRun(id: string): Promise<void>
  ResumeRun(id: string): Promise<void>
  DeleteRun(id: string): Promise<void>
  ListProjects(): Promise<Project[]>
  CreateProject(project: Project): Promise<Project>
  Logs(id: string, container: string): Promise<string>
  StartLogStream(id: string, container: string): Promise<string>
  StopLogStream(streamID: string): Promise<void>
}

declare global {
  interface Window {
    go?: { desktop?: { App?: Backend } }
    runtime?: { EventsOn(eventName: string, callback: (event: LogEvent) => void): () => void }
  }
}

function backend(): Backend {
  const app = window.go?.desktop?.App
  if (!app) throw new Error("Wren desktop bridge is unavailable. Start this UI through the Wails application.")
  return app
}

export const api = {
  load: () => backend().Load(),
  selectContext: (name: string) => backend().SelectContext(name),
  saveContext: (context: { name: string; server: string; org?: string; user?: string; token?: string }) => backend().SaveContext(context),
  listRuns: (scope: string, project: string, phase: string) => backend().ListRuns(scope, project, phase),
  getRun: (id: string) => backend().GetRun(id),
  createRun: (options: RunCreate) => backend().CreateRun(options),
  stopRun: (id: string) => backend().StopRun(id),
  resumeRun: (id: string) => backend().ResumeRun(id),
  deleteRun: (id: string) => backend().DeleteRun(id),
  listProjects: () => backend().ListProjects(),
  createProject: (project: Project) => backend().CreateProject(project),
  logs: (id: string, container: string) => backend().Logs(id, container),
  startLogStream: (id: string, container: string) => backend().StartLogStream(id, container),
  stopLogStream: (streamID: string) => backend().StopLogStream(streamID),
  onLog: (callback: (event: LogEvent) => void) => {
    if (!window.runtime) throw new Error("Wren event bridge is unavailable.")
    return window.runtime.EventsOn("wren:log", callback)
  },
}
