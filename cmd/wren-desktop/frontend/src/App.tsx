import { FormEvent, useCallback, useEffect, useMemo, useState } from "react"
import { api, Bootstrap, Project, Run, RunCreate, RunEvent } from "./api"
import { canFollowRunLogs, logPlaceholder } from "./runState"
import { displayLogLines } from "./logLines"

const containers = ["harness", "hydrate", "egress-proxy", "checkpointer", "agent-gateway", "egress-lockdown"]
const harnesses = ["claude-code", "codex", "opencode", "mock", "byo"]
const scopes = [{ value: "all", label: "Everyone" }, { value: "mine", label: "My runs" }, { value: "team", label: "Team" }]
const phases = [
  { value: "", label: "All states" },
  { value: "Running", label: "Running" },
  { value: "Paused", label: "Paused" },
  { value: "Failed", label: "Needs attention" },
  { value: "Succeeded", label: "Completed" },
]

function errText(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}

function relativeTime(value?: string) {
  if (!value) return "—"
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 1000))
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`
  return `${Math.floor(seconds / 86400)}d`
}

function phaseTone(phase: string) {
  if (phase === "Succeeded") return "success"
  if (phase === "Failed" || phase === "Canceled") return "danger"
  if (phase === "Running" || phase === "Finalizing" || phase === "Pausing") return "active"
  return "quiet"
}

function eventSummary(event: RunEvent) {
  const payload = event.payload || {}
  if (event.type === "status" && typeof payload.phase === "string") return `Agent entered ${payload.phase}`
  if (event.type === "message" && typeof payload.message === "string") return payload.message
  if (event.type === "tool_call" && typeof payload.tool === "string") return `Tool · ${payload.tool}`
  if (event.type === "error" && typeof payload.error === "string") return payload.error
  if (event.type === "run.snapshot" && typeof payload.phase === "string") return `Control plane observed ${payload.phase}`
  if (event.type === "run.submitted") return "Run accepted and recorded durably"
  if (event.type === "run.launch_accepted") return "Kubernetes accepted the run"
  return event.type.replaceAll("_", " ").replaceAll(".", " · ")
}

export function App() {
  const [data, setData] = useState<Bootstrap>({ contexts: [], projects: [], runs: [] })
  const [selected, setSelected] = useState<string>("")
  const [view, setView] = useState<"fleet" | "projects">("fleet")
  const [scope, setScope] = useState("all")
  const [projectFilter, setProjectFilter] = useState("")
  const [phaseFilter, setPhaseFilter] = useState("")
  const [busy, setBusy] = useState(true)
  const [error, setError] = useState("")
  const [composer, setComposer] = useState(false)
  const [contextModal, setContextModal] = useState(false)

  const applyBootstrap = useCallback((next: Bootstrap) => {
    setData(next)
    setError(next.warning || "")
  }, [])

  const load = useCallback(async () => {
    try {
      setError("")
      const next = await api.load()
      applyBootstrap(next)
      if (!selected && next.runs[0]) setSelected(next.runs[0].id)
      if (next.contexts.length === 0) setContextModal(true)
    } catch (e) { setError(errText(e)) }
    finally { setBusy(false) }
  }, [applyBootstrap, selected])

  const refreshRuns = useCallback(async () => {
    if (!data.contexts.length) return
    try {
      const runs = await api.listRuns(scope, projectFilter, phaseFilter)
      setData(current => ({ ...current, runs }))
      setError("")
    } catch (e) { setError(errText(e)) }
  }, [data.contexts.length, scope, projectFilter, phaseFilter])

  useEffect(() => { void load() }, [])
  useEffect(() => {
    if (!data.contexts.length) return
    void refreshRuns()
    const timer = window.setInterval(refreshRuns, 3000)
    return () => window.clearInterval(timer)
  }, [refreshRuns, data.contexts.length])

  const selectedRun = useMemo(() => data.runs.find(run => run.id === selected), [data.runs, selected])
  const counts = useMemo(() => ({
    running: data.runs.filter(r => r.phase === "Running" || r.phase === "Finalizing").length,
    failed: data.runs.filter(r => r.phase === "Failed").length,
    total: data.runs.length,
  }), [data.runs])

  async function mutate(action: () => Promise<unknown>) {
    try { setBusy(true); setError(""); await action(); await refreshRuns() }
    catch (e) { setError(errText(e)) }
    finally { setBusy(false) }
  }

  return <div className="shell">
    <aside className="rail">
      <div className="brand"><span className="mark">W</span><span>Wren</span></div>
      <nav>
        <button className={view === "fleet" ? "nav active" : "nav"} onClick={() => setView("fleet")}><span>◫</span> Fleet</button>
        <button className={view === "projects" ? "nav active" : "nav"} onClick={() => setView("projects")}><span>⌘</span> Projects</button>
      </nav>
      <div className="rail-spacer" />
      <button className="context-card" onClick={() => setContextModal(true)}>
        <span className="status-dot" />
        <span><small>Control plane</small><strong>{data.contexts.find(c => c.selected)?.name ?? "Not connected"}</strong></span>
        <span>⌄</span>
      </button>
    </aside>

    <main>
      <header className="topbar">
        <div><h1>{view === "fleet" ? "Agent fleet" : "Projects"}</h1><p>{view === "fleet" ? "Every task, agent, and outcome across your software factory." : "GitHub repositories and their orchestration defaults."}</p></div>
        <div className="top-actions">
          <button className="ghost" onClick={() => void (view === "fleet" ? refreshRuns() : load())}>↻ Refresh</button>
          <button className="primary" onClick={() => setComposer(true)}>＋ New run</button>
        </div>
      </header>

      {error && <div className="error-banner"><span>!</span>{error}<button onClick={() => setError("")}>×</button></div>}

      {view === "fleet" ? <>
        <section className="metrics">
          <div><small>Visible runs</small><strong>{counts.total}</strong></div>
          <div><small>Running now</small><strong className="green">{counts.running}</strong></div>
          <div><small>Needs attention</small><strong className={counts.failed ? "red" : ""}>{counts.failed}</strong></div>
        </section>
        <section className="toolbar" aria-label="Fleet filters">
          <FilterGroup label="Scope" value={scope} options={scopes} onChange={setScope} />
          <FilterGroup label="Project" value={projectFilter} options={[{ value: "", label: "All projects" }, ...data.projects.map(p => ({ value: p.name, label: p.name }))]} onChange={setProjectFilter} scroll />
          <FilterGroup label="State" value={phaseFilter} options={phases} onChange={setPhaseFilter} />
          <span className="live"><i /> Live · 3s</span>
        </section>
        <div className="workspace">
          <section className="run-list">
            <div className="list-head"><span>Run</span><span>Project</span><span>State</span><span>Age</span></div>
            {data.runs.map(run => <button key={run.id} className={selected === run.id ? "run-row selected" : "run-row"} onClick={() => setSelected(run.id)}>
              <span><strong>{run.id}</strong><small>{run.user || "unknown user"}</small></span>
              <span>{run.project}<small>{run.harness || "default harness"}</small></span>
              <span><b className={`phase ${phaseTone(run.phase)}`}>{run.phase}</b><small>{run.restartCount || 0} restarts</small></span>
              <span className="age">{relativeTime(run.createdAt)}</span>
            </button>)}
            {!busy && data.runs.length === 0 && <div className="empty"><span>◇</span><h3>No runs match</h3><p>Launch a feature or adjust the fleet filters.</p></div>}
          </section>
          <RunDetail run={selectedRun} onAction={mutate} onDelete={() => { setSelected(""); void refreshRuns() }} />
        </div>
      </> : <Projects projects={data.projects} onCreated={load} />}
    </main>

    {composer && <RunComposer projects={data.projects} onClose={() => setComposer(false)} onCreated={run => { setComposer(false); setSelected(run.id); setView("fleet"); void refreshRuns() }} />}
    {contextModal && <ContextModal contexts={data.contexts} onClose={() => data.contexts.length && setContextModal(false)} onLoaded={next => { applyBootstrap(next); setContextModal(false) }} />}
    {busy && <div className="busy"><span /></div>}
  </div>
}

function FilterGroup({ label, value, options, onChange, scroll = false }: { label: string; value: string; options: { value: string; label: string }[]; onChange: (value: string) => void; scroll?: boolean }) {
  return <div className={`filter-group ${scroll ? "scroll" : ""}`}>
    <span className="filter-label">{label}</span>
    <div className="choice-row" role="group" aria-label={label}>
      {options.map(option => <button key={`${label}-${option.value}`} type="button" className={value === option.value ? "choice active" : "choice"} aria-pressed={value === option.value} onClick={() => onChange(option.value)}>{option.label}</button>)}
    </div>
  </div>
}

function RunDetail({ run, onAction, onDelete }: { run?: Run; onAction: (a: () => Promise<unknown>) => Promise<void>; onDelete: () => void }) {
  const [tab, setTab] = useState<"overview" | "logs" | "recovery">("logs")
  const [logs, setLogs] = useState("")
  const [container, setContainer] = useState("harness")
  const [logError, setLogError] = useState("")
  const [logLive, setLogLive] = useState(false)
	const [events, setEvents] = useState<RunEvent[]>([])
	const [eventError, setEventError] = useState("")
  const [confirmingDelete, setConfirmingDelete] = useState(false)
  useEffect(() => {
    setLogs(""); setLogError("")
  }, [run?.id, container])
  useEffect(() => {
    setLogError(""); setLogLive(false)
    if (!run?.id || !canFollowRunLogs(run.phase)) return
    let disposed = false
    let streamID = ""
    const unsubscribe = api.onLog(event => {
      if (disposed || (streamID && event.streamId !== streamID)) return
      if (event.chunk) setLogs(current => (current + event.chunk).slice(-500_000))
      if (event.error) setLogError(event.error)
      if (event.done) setLogLive(false)
    })
    void api.startLogStream(run.id, container).then(id => {
      streamID = id
      if (disposed) void api.stopLogStream(id)
      else setLogLive(true)
    }).catch(error => { if (!disposed) setLogError(errText(error)) })
    return () => {
      disposed = true
      unsubscribe()
      if (streamID) void api.stopLogStream(streamID)
    }
  }, [run?.id, run?.phase, container])
  useEffect(() => {
    setEvents([]); setEventError("")
    if (!run?.id || tab !== "recovery") return
    let disposed = false
    const refresh = async () => {
      try {
        const next = await api.listRunEvents(run.id, 0, 200)
        if (!disposed) { setEvents(next); setEventError("") }
      } catch (error) {
        if (!disposed) setEventError(errText(error))
      }
    }
    void refresh()
    const timer = window.setInterval(refresh, 3000)
    return () => { disposed = true; window.clearInterval(timer) }
  }, [run?.id, tab])
  if (!run) return <aside className="detail empty-detail"><span>◇</span><h3>Select a run</h3><p>Inspect state, logs, pull requests, and lifecycle actions.</p></aside>
  const runID = run.id
  async function fetchLogs() {
    try { setLogError(""); setLogs(await api.logs(runID, container)) } catch (e) { setLogError(errText(e)) }
  }
  return <aside className="detail">
    <div className="detail-head"><div><b className={`phase ${phaseTone(run.phase)}`}>{run.phase}</b><h2>{run.id}</h2><p>{run.project} · {run.harness || "default"}</p></div><button className="icon">•••</button></div>
    <div className="detail-tabs" role="tablist">
      {(["overview", "logs", "recovery"] as const).map(value => <button key={value} role="tab" aria-selected={tab === value} className={tab === value ? "active" : ""} onClick={() => setTab(value)}>{value === "logs" ? "Live logs" : value[0].toUpperCase() + value.slice(1)}</button>)}
    </div>
    {tab === "overview" && <div className="detail-panel overview-panel">
      <dl><div><dt>Owner</dt><dd>{run.user || "—"}</dd></div><div><dt>Restarts</dt><dd>{run.restartCount || 0}</dd></div><div><dt>Namespace</dt><dd title={run.namespace}>{run.namespace || "—"}</dd></div><div><dt>Created</dt><dd>{run.createdAt ? new Date(run.createdAt).toLocaleString() : "—"}</dd></div></dl>
      {run.prUrl ? <a className="pr-card" href={run.prUrl} target="_blank"><span>⑂</span><span><small>Pull request ready</small><strong>{run.prUrl.replace("https://github.com/", "")}</strong></span><b>↗</b></a> : <div className="quiet-card"><strong>No pull request yet</strong><span>Wren will attach the pull request when finalization completes.</span></div>}
    </div>}
    {tab === "logs" && <div className="detail-panel logs-panel">
      <div className="logs-head"><div><h3>Container output</h3>{logLive && <span className="live"><i /> Streaming live</span>}</div><button className="ghost compact" onClick={() => void fetchLogs()}>Refresh snapshot</button></div>
      <div className="container-tabs" role="tablist" aria-label="Log container">{containers.map(value => <button key={value} role="tab" aria-selected={container === value} className={container === value ? "active" : ""} onClick={() => setContainer(value)}>{value}</button>)}</div>
      <div className="logs" tabIndex={0} role="log" aria-label={`${container} container output`}>
        {logError
          ? <div className="log-state error"><strong>Unable to stream logs</strong><span>{logError}</span></div>
          : logs
            ? displayLogLines(logs).map(line => <div className={`log-line ${line.kind}`} key={line.id}><time>{line.time || "··:··:··"}</time><span className="log-kind">{line.kind}</span><code>{line.message}</code></div>)
            : <div className="log-state"><span>{logPlaceholder(run.phase)}</span></div>}
      </div>
    </div>}
    {tab === "recovery" && <div className="detail-panel recovery-panel">
      {run.lastCheckpoint ? <div className="checkpoint-card"><small>Durable checkpoint · {run.lastCheckpoint.trigger || "recovery"}</small><strong>{run.lastCheckpoint.id}</strong><span>{run.lastCheckpoint.at ? new Date(run.lastCheckpoint.at).toLocaleString() : "—"} · {run.lastCheckpoint.sizeBytes ? `${Math.ceil(run.lastCheckpoint.sizeBytes / 1024)} KiB` : "—"}</span><code>{run.lastCheckpoint.sha256 ? `sha256:${run.lastCheckpoint.sha256.slice(0, 20)}…` : run.lastCheckpoint.uri}</code></div> : <div className="quiet-card"><strong>No durable checkpoint recorded</strong><span>Checkpoint metadata will appear here once a verified snapshot is published.</span></div>}
      {!!run.conditions?.length && <div className="conditions"><h3>Recovery timeline</h3>{run.conditions.filter(c => c.type !== "Ready" && c.type !== "EgressEnforcement").map(c => <div key={c.type}><span className={`status-dot ${c.status === "False" ? "warn" : ""}`} /><span><strong>{c.type}</strong><small>{c.reason}{c.message ? ` · ${c.message}` : ""}</small></span></div>)}</div>}
	  <div className="event-journal"><div className="journal-head"><span><small>Durable journal</small><strong>{events.length} events</strong></span><i className="status-dot" /></div>
		{eventError && <div className="journal-error">{eventError}</div>}
		{events.length ? <ol>{[...events].reverse().map(event => <li key={event.id}><time>{new Date(event.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</time><span><strong>{eventSummary(event)}</strong><small>{event.source} · {event.sourceId}</small></span></li>)}</ol> : !eventError && <div className="journal-empty">Waiting for the first durable event…</div>}
	  </div>
    </div>}
    <div className="lifecycle">
      {(run.phase === "Running" || run.phase === "Pending" || run.phase === "Provisioning") && <button onClick={() => void onAction(() => api.stopRun(runID))}>Stop run</button>}
      {run.phase === "Running" && <button className="primary" onClick={() => void onAction(() => api.pauseRun(runID))}>Pause safely</button>}
      {(run.phase === "Failed" || run.phase === "Paused") && <button className="primary" onClick={() => void onAction(() => api.resumeRun(runID))}>Resume run</button>}
      <button className="danger-button" onClick={() => setConfirmingDelete(true)}>Delete</button>
    </div>
    {confirmingDelete && <div className="modal-backdrop" role="presentation">
      <div className="modal confirm-modal" role="alertdialog" aria-modal="true" aria-labelledby="delete-run-title" aria-describedby="delete-run-description">
        <div className="modal-head"><div><small>Destructive action</small><h2 id="delete-run-title">Delete run?</h2></div><button type="button" aria-label="Close" onClick={() => setConfirmingDelete(false)}>×</button></div>
        <p id="delete-run-description">Delete <strong>{runID}</strong> and its workspace. This cannot be undone.</p>
        <div className="modal-actions"><button type="button" className="ghost" onClick={() => setConfirmingDelete(false)}>Cancel</button><button type="button" className="danger-button" onClick={() => { setConfirmingDelete(false); void onAction(async () => { await api.deleteRun(runID); onDelete() }) }}>Delete run</button></div>
      </div>
    </div>}
  </aside>
}

function RunComposer({ projects, onClose, onCreated }: { projects: Project[]; onClose: () => void; onCreated: (run: Run) => void }) {
  const [form, setForm] = useState<RunCreate>({ project: projects[0]?.name || "", task: "" })
  const [error, setError] = useState("")
  const [saving, setSaving] = useState(false)
  async function submit(e: FormEvent) {
    e.preventDefault(); setSaving(true); setError("")
    try { onCreated(await api.createRun(form)) } catch (err) { setError(errText(err)); setSaving(false) }
  }
  return <div className="modal-backdrop"><form className="modal composer" onSubmit={submit}>
    <div className="modal-head"><div><small>New orchestration</small><h2>What should Wren build?</h2></div><button type="button" onClick={onClose}>×</button></div>
    <fieldset className="choice-field"><legend>Project</legend><div className="card-choices">{projects.map(project => <button key={project.name} type="button" className={form.project === project.name ? "choice-card active" : "choice-card"} onClick={() => setForm({ ...form, project: project.name })}><strong>{project.name}</strong><span>{project.repo || "Keyless project"}</span></button>)}</div></fieldset>
    <label>Task<textarea autoFocus required rows={8} placeholder="Describe the feature, bug, constraints, and definition of done…" value={form.task} onChange={e => setForm({ ...form, task: e.target.value })} /></label>
    <fieldset className="choice-field"><legend>Harness</legend><div className="choice-row wrap"><button type="button" className={!form.harness ? "choice active" : "choice"} onClick={() => setForm({ ...form, harness: "" })}>Project default</button>{harnesses.map(harness => <button key={harness} type="button" className={form.harness === harness ? "choice active" : "choice"} onClick={() => setForm({ ...form, harness })}>{harness}</button>)}</div></fieldset>
    <label>Base branch<input placeholder="main" value={form.baseRef || ""} onChange={e => setForm({ ...form, baseRef: e.target.value })} /></label>
    {error && <p className="form-error">{error}</p>}
    <div className="modal-actions"><button type="button" className="ghost" onClick={onClose}>Cancel</button><button className="primary" disabled={saving}>{saving ? "Launching…" : "Launch agent"}</button></div>
  </form></div>
}

function Projects({ projects, onCreated }: { projects: Project[]; onCreated: () => void }) {
  const [adding, setAdding] = useState(false)
  return <section className="project-page">
    <div className="project-grid">{projects.map(p => <article key={p.name}><div className="repo-icon">⑂</div><div><h3>{p.name}</h3><p>{p.repo || "Keyless project"}</p></div><span>{p.defaultHarness || "default"}</span><dl><div><dt>CPU</dt><dd>{p.cpu || "default"}</dd></div><div><dt>Memory</dt><dd>{p.memory || "default"}</dd></div><div><dt>Disk</dt><dd>{p.disk || "default"}</dd></div></dl></article>)}</div>
    <button className="add-project" onClick={() => setAdding(true)}>＋ Register project</button>
    {adding && <ProjectModal onClose={() => setAdding(false)} onCreated={() => { setAdding(false); onCreated() }} />}
  </section>
}

function ProjectModal({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [project, setProject] = useState<Project>({ name: "", repo: "", defaultHarness: "claude-code" })
  const [error, setError] = useState("")
  async function submit(e: FormEvent) { e.preventDefault(); try { await api.createProject(project); onCreated() } catch (err) { setError(errText(err)) } }
  return <div className="modal-backdrop"><form className="modal" onSubmit={submit}><div className="modal-head"><h2>Register project</h2><button type="button" onClick={onClose}>×</button></div><div className="form-grid"><label>Name<input required placeholder="payments-api" value={project.name} onChange={e => setProject({ ...project, name: e.target.value })} /></label><label>GitHub repository<input placeholder="acme/payments-api" value={project.repo || ""} onChange={e => setProject({ ...project, repo: e.target.value })} /></label><label>Namespace<input placeholder="wren-runs" value={project.namespace || ""} onChange={e => setProject({ ...project, namespace: e.target.value })} /></label></div><fieldset className="choice-field"><legend>Default harness</legend><div className="choice-row wrap">{harnesses.map(harness => <button key={harness} type="button" className={project.defaultHarness === harness ? "choice active" : "choice"} onClick={() => setProject({ ...project, defaultHarness: harness })}>{harness}</button>)}</div></fieldset>{error && <p className="form-error">{error}</p>}<div className="modal-actions"><button type="button" className="ghost" onClick={onClose}>Cancel</button><button className="primary">Register</button></div></form></div>
}

function ContextModal({ contexts, onClose, onLoaded }: { contexts: Bootstrap["contexts"]; onClose: () => void; onLoaded: (data: Bootstrap) => void }) {
  const [adding, setAdding] = useState(contexts.length === 0)
  const [form, setForm] = useState({ name: "", server: "", user: "" })
  const [error, setError] = useState("")
  async function select(name: string) { try { onLoaded(await api.selectContext(name)) } catch (e) { setError(errText(e)) } }
  async function save(e: FormEvent) { e.preventDefault(); try { onLoaded(await api.saveContext(form)) } catch (err) { setError(errText(err)) } }
  return <div className="modal-backdrop"><div className="modal context-modal"><div className="modal-head"><div><small>Connections</small><h2>Control planes</h2></div>{contexts.length > 0 && <button onClick={onClose}>×</button>}</div>{!adding && <div className="context-list">{contexts.map(c => <button key={c.name} onClick={() => void select(c.name)}><span className="status-dot"/><span><strong>{c.name}</strong><small>{c.server} · {c.user || "default user"}</small></span>{c.selected && <b>Current</b>}</button>)}<button className="add-context" onClick={() => setAdding(true)}>＋ Add control plane</button></div>}{adding && <form onSubmit={save}><label>Name<input required placeholder="production" value={form.name} onChange={e => setForm({ ...form, name: e.target.value })}/></label><label>Server<input required placeholder="https://wren.example.com" value={form.server} onChange={e => setForm({ ...form, server: e.target.value })}/></label><label>User<input placeholder="you@company.com" value={form.user} onChange={e => setForm({ ...form, user: e.target.value })}/></label><div className="modal-actions">{contexts.length > 0 && <button type="button" className="ghost" onClick={() => setAdding(false)}>Back</button>}<button className="primary">Connect</button></div></form>}{error && <p className="form-error">{error}</p>}</div></div>
}
