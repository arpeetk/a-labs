import { FormEvent, useCallback, useEffect, useMemo, useState } from "react"
import { api, Bootstrap, Project, Run, RunCreate } from "./api"

const phases = ["", "Pending", "Provisioning", "Running", "Interrupted", "Succeeded", "Failed", "Canceled"]
const containers = ["harness", "hydrate", "egress-proxy", "checkpointer", "agent-gateway", "egress-lockdown"]

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
  if (phase === "Running" || phase === "Finalizing") return "active"
  return "quiet"
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

  const load = useCallback(async () => {
    try {
      setError("")
      const next = await api.load()
      setData(next)
      if (!selected && next.runs[0]) setSelected(next.runs[0].id)
      if (next.contexts.length === 0) setContextModal(true)
    } catch (e) { setError(errText(e)) }
    finally { setBusy(false) }
  }, [selected])

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
        <section className="toolbar">
          <select value={scope} onChange={e => setScope(e.target.value)}><option value="all">Everyone</option><option value="mine">My runs</option><option value="team">Team</option></select>
          <select value={projectFilter} onChange={e => setProjectFilter(e.target.value)}><option value="">All projects</option>{data.projects.map(p => <option key={p.name}>{p.name}</option>)}</select>
          <select value={phaseFilter} onChange={e => setPhaseFilter(e.target.value)}>{phases.map(p => <option key={p} value={p}>{p || "All phases"}</option>)}</select>
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
    {contextModal && <ContextModal contexts={data.contexts} onClose={() => data.contexts.length && setContextModal(false)} onLoaded={next => { setData(next); setContextModal(false) }} />}
    {busy && <div className="busy"><span /></div>}
  </div>
}

function RunDetail({ run, onAction, onDelete }: { run?: Run; onAction: (a: () => Promise<unknown>) => Promise<void>; onDelete: () => void }) {
  const [logs, setLogs] = useState("")
  const [container, setContainer] = useState("harness")
  const [logError, setLogError] = useState("")
  const [logLive, setLogLive] = useState(false)
  useEffect(() => {
    setLogs(""); setLogError(""); setLogLive(false)
    if (!run?.id) return
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
  }, [run?.id, container])
  if (!run) return <aside className="detail empty-detail"><span>◇</span><h3>Select a run</h3><p>Inspect state, logs, pull requests, and lifecycle actions.</p></aside>
  const runID = run.id
  async function fetchLogs() {
    try { setLogError(""); setLogs(await api.logs(runID, container)) } catch (e) { setLogError(errText(e)) }
  }
  return <aside className="detail">
    <div className="detail-head"><div><b className={`phase ${phaseTone(run.phase)}`}>{run.phase}</b><h2>{run.id}</h2><p>{run.project} · {run.harness || "default"}</p></div><button className="icon">•••</button></div>
    <dl><div><dt>Owner</dt><dd>{run.user || "—"}</dd></div><div><dt>Restarts</dt><dd>{run.restartCount || 0}</dd></div><div><dt>Namespace</dt><dd>{run.namespace || "—"}</dd></div><div><dt>Created</dt><dd>{run.createdAt ? new Date(run.createdAt).toLocaleString() : "—"}</dd></div></dl>
    {run.prUrl && <a className="pr-card" href={run.prUrl} target="_blank"><span>⑂</span><span><small>Pull request ready</small><strong>{run.prUrl.replace("https://github.com/", "")}</strong></span><b>↗</b></a>}
    <div className="logs-head"><h3>Run logs {logLive && <span className="live"><i /> Live</span>}</h3><select value={container} onChange={e => setContainer(e.target.value)}>{containers.map(c => <option key={c}>{c}</option>)}</select><button className="ghost compact" onClick={() => void fetchLogs()}>Snapshot</button></div>
    <pre className="logs">{logError ? `Unable to stream logs\n${logError}` : logs || "Waiting for container output…"}</pre>
    <div className="lifecycle">
      {(run.phase === "Running" || run.phase === "Pending" || run.phase === "Provisioning") && <button onClick={() => void onAction(() => api.stopRun(runID))}>Stop run</button>}
      {run.phase === "Failed" && <button className="primary" onClick={() => void onAction(() => api.resumeRun(runID))}>Resume run</button>}
      <button className="danger-button" onClick={() => { if (confirm(`Delete ${runID} and its workspace?`)) void onAction(async () => { await api.deleteRun(runID); onDelete() }) }}>Delete</button>
    </div>
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
    <label>Project<select required value={form.project} onChange={e => setForm({ ...form, project: e.target.value })}><option value="" disabled>Select a repository</option>{projects.map(p => <option key={p.name}>{p.name}</option>)}</select></label>
    <label>Task<textarea autoFocus required rows={8} placeholder="Describe the feature, bug, constraints, and definition of done…" value={form.task} onChange={e => setForm({ ...form, task: e.target.value })} /></label>
    <div className="form-grid"><label>Harness<select value={form.harness || ""} onChange={e => setForm({ ...form, harness: e.target.value })}><option value="">Project default</option><option>claude-code</option><option>codex</option><option>opencode</option><option>mock</option><option>byo</option></select></label><label>Base branch<input placeholder="main" value={form.baseRef || ""} onChange={e => setForm({ ...form, baseRef: e.target.value })} /></label></div>
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
  return <div className="modal-backdrop"><form className="modal" onSubmit={submit}><div className="modal-head"><h2>Register project</h2><button type="button" onClick={onClose}>×</button></div><div className="form-grid"><label>Name<input required placeholder="payments-api" value={project.name} onChange={e => setProject({ ...project, name: e.target.value })} /></label><label>GitHub repository<input placeholder="acme/payments-api" value={project.repo || ""} onChange={e => setProject({ ...project, repo: e.target.value })} /></label><label>Default harness<select value={project.defaultHarness} onChange={e => setProject({ ...project, defaultHarness: e.target.value })}><option>claude-code</option><option>codex</option><option>opencode</option><option>mock</option><option>byo</option></select></label><label>Namespace<input placeholder="wren-runs" value={project.namespace || ""} onChange={e => setProject({ ...project, namespace: e.target.value })} /></label></div>{error && <p className="form-error">{error}</p>}<div className="modal-actions"><button type="button" className="ghost" onClick={onClose}>Cancel</button><button className="primary">Register</button></div></form></div>
}

function ContextModal({ contexts, onClose, onLoaded }: { contexts: Bootstrap["contexts"]; onClose: () => void; onLoaded: (data: Bootstrap) => void }) {
  const [adding, setAdding] = useState(contexts.length === 0)
  const [form, setForm] = useState({ name: "", server: "", user: "" })
  const [error, setError] = useState("")
  async function select(name: string) { try { onLoaded(await api.selectContext(name)) } catch (e) { setError(errText(e)) } }
  async function save(e: FormEvent) { e.preventDefault(); try { onLoaded(await api.saveContext(form)) } catch (err) { setError(errText(err)) } }
  return <div className="modal-backdrop"><div className="modal context-modal"><div className="modal-head"><div><small>Connections</small><h2>Control planes</h2></div>{contexts.length > 0 && <button onClick={onClose}>×</button>}</div>{!adding && <div className="context-list">{contexts.map(c => <button key={c.name} onClick={() => void select(c.name)}><span className="status-dot"/><span><strong>{c.name}</strong><small>{c.server} · {c.user || "default user"}</small></span>{c.selected && <b>Current</b>}</button>)}<button className="add-context" onClick={() => setAdding(true)}>＋ Add control plane</button></div>}{adding && <form onSubmit={save}><label>Name<input required placeholder="production" value={form.name} onChange={e => setForm({ ...form, name: e.target.value })}/></label><label>Server<input required placeholder="https://wren.example.com" value={form.server} onChange={e => setForm({ ...form, server: e.target.value })}/></label><label>User<input placeholder="you@company.com" value={form.user} onChange={e => setForm({ ...form, user: e.target.value })}/></label><div className="modal-actions">{contexts.length > 0 && <button type="button" className="ghost" onClick={() => setAdding(false)}>Back</button>}<button className="primary">Connect</button></div></form>}{error && <p className="form-error">{error}</p>}</div></div>
}
