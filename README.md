# a-labs — Clinical Documentation System

Real-time clinical documentation powered by Whisper ASR + Claude. Captures doctor-patient
conversations, produces structured SOAP notes with linked evidence (every claim grounded
to a transcript quote), and streams the note back to the clinician within seconds of visit end.

## Architecture

```
Mobile (React Native/Expo)
  │  WebSocket: binary PCM audio chunks (3s each)
  │  REST: create visit, fetch note
  ▼
Backend (FastAPI)
  │  Buffer audio → Whisper API (transcription, real-time deltas)
  │  Full transcript → Claude claude-sonnet-4-6 (SOAP note + evidence)
  │  Persist in SQLite (dev) / PostgreSQL (prod)
  ▼
AI Pipeline
  ├── ASR: OpenAI Whisper-1 (verbose JSON, segment timestamps)
  └── Notes: Anthropic Claude (streaming JSON, hallucination self-check)
```

### Key design decisions (Abridge-style)

| Decision | Rationale |
|---|---|
| **Chunked WebSocket streaming** | Send audio every 3 s → Whisper → transcript deltas appear in real time; clinician sees progress during long visits |
| **Linked evidence** | Every SOAP section carries an `evidence` field: exact transcript substring. Makes hallucinations auditable. |
| **Self-reported hallucination check** | Claude rates its own confabulation risk (none/low/medium/high). High → flag for human review. |
| **Streaming note generation** | Claude's response streams token-by-token to the mobile client so the note appears progressively, not all at once. |
| **SQLite dev / PostgreSQL prod** | Single `DATABASE_URL` env var; swap without code change. |

## Setup

### Backend

```bash
cd backend
cp .env.example .env
# Fill in OPENAI_API_KEY and ANTHROPIC_API_KEY

pip install -r requirements.txt
uvicorn app.main:app --reload --port 8080
```

Or with Docker:

```bash
docker compose up --build
```

API docs at `http://localhost:8080/docs`

### Mobile

```bash
cd mobile
npm install
npx expo start
```

Scan the QR code with Expo Go on your phone, or press `i` for iOS simulator / `a` for Android emulator.

If testing on a physical device, update `src/config.ts` with your machine's LAN IP.

## API reference

| Method | Path | Description |
|---|---|---|
| `POST` | `/visits` | Create a visit (returns `id`) |
| `GET` | `/visits` | List recent visits |
| `GET` | `/visits/{id}` | Get visit + note |
| `POST` | `/visits/{id}/finalize` | Re-generate note from saved transcript |
| `WS` | `/ws/visits/{id}/audio` | Stream PCM audio; send `"END"` to finalize |

### WebSocket message protocol

**Client → Server**
- Binary frames: raw 16-bit mono PCM @ 16 kHz or 48 kHz
- Text `"END"`: signals end of recording

**Server → Client**
```json
{"type": "transcript_delta", "text": "...partial segment..."}
{"type": "transcript_final", "text": "...full transcript..."}
{"type": "note_delta",       "text": "...streaming JSON token..."}
{"type": "note_complete",    "note": { ...structured SOAP note... }}
{"type": "error",            "message": "..."}
```

## SOAP note schema

```json
{
  "subjective": {
    "chief_complaint":              {"text": "...", "evidence": "exact transcript quote"},
    "history_of_present_illness":   {"text": "...", "evidence": "exact transcript quote"},
    "review_of_systems":            {"text": "...", "evidence": "exact transcript quote"}
  },
  "objective": {
    "vitals":       {"text": "...", "evidence": "..."},
    "physical_exam":{"text": "...", "evidence": "..."}
  },
  "assessment": {
    "diagnosis":    {"text": "...", "evidence": "..."},
    "differential": {"text": "...", "evidence": "..."}
  },
  "plan": {
    "treatment":            {"text": "...", "evidence": "..."},
    "follow_up":            {"text": "...", "evidence": "..."},
    "patient_instructions": {"text": "...", "evidence": "..."}
  },
  "hallucination_check": "none | low | medium | high"
}
```
