"""
WebSocket endpoint: ws://host/ws/visits/{visit_id}/audio

Protocol:
  Client → Server: binary frames of raw 16-bit mono PCM audio at 48 kHz.
                   Send the text message "END" to signal end of recording.
  Server → Client: JSON text frames with transcript deltas and final note.

Frame types sent by server:
  {"type": "transcript_delta", "text": "...partial segment..."}
  {"type": "transcript_final", "text": "...full transcript..."}
  {"type": "note_delta",       "text": "...streaming JSON token..."}
  {"type": "note_complete",    "note": {...structured SOAP note...}}
  {"type": "error",            "message": "..."}
"""

import asyncio
import json

from fastapi import APIRouter, WebSocket, WebSocketDisconnect
from sqlalchemy.ext.asyncio import AsyncSession

from ..config import settings
from ..database import AsyncSessionLocal
from ..models import Visit, VisitStatus
from ..services import generate_soap_note, transcribe

router = APIRouter(tags=["stream"])

# visit_id → accumulated PCM bytes (before flushing to ASR)
_audio_buffers: dict[int, bytearray] = {}
# visit_id → assembled transcript segments
_transcripts: dict[int, list[str]] = {}


async def _flush_buffer(visit_id: int, websocket: WebSocket) -> str | None:
    """Send buffered PCM to Whisper, broadcast transcript delta, return segment text."""
    buf = _audio_buffers.get(visit_id)
    if not buf or len(buf) < 3200:  # skip tiny fragments
        return None

    pcm = bytes(buf)
    _audio_buffers[visit_id] = bytearray()

    try:
        text = await transcribe(pcm)
    except Exception as exc:
        await websocket.send_text(json.dumps({"type": "error", "message": str(exc)}))
        return None

    if text.strip():
        _transcripts.setdefault(visit_id, []).append(text.strip())
        await websocket.send_text(json.dumps({"type": "transcript_delta", "text": text.strip()}))
    return text.strip() or None


@router.websocket("/ws/visits/{visit_id}/audio")
async def audio_stream(visit_id: int, websocket: WebSocket):
    await websocket.accept()
    _audio_buffers[visit_id] = bytearray()
    _transcripts[visit_id] = []

    async with AsyncSessionLocal() as db:
        visit = await db.get(Visit, visit_id)
        if not visit:
            await websocket.send_text(json.dumps({"type": "error", "message": "Visit not found"}))
            await websocket.close()
            return

        visit.status = VisitStatus.recording
        await db.commit()

    try:
        while True:
            message = await websocket.receive()

            if message["type"] == "websocket.disconnect":
                break

            # Text "END" → finalize transcription + generate note
            if message.get("text") == "END":
                # Flush any remaining audio
                await _flush_buffer(visit_id, websocket)

                full_transcript = " ".join(_transcripts.get(visit_id, []))

                async with AsyncSessionLocal() as db:
                    visit = await db.get(Visit, visit_id)
                    visit.transcript = full_transcript
                    visit.status = VisitStatus.generating
                    await db.commit()

                await websocket.send_text(
                    json.dumps({"type": "transcript_final", "text": full_transcript})
                )

                # Stream SOAP note generation token-by-token
                from ..services.note_generator import stream_soap_note

                raw_json = ""
                async for token in stream_soap_note(full_transcript):
                    raw_json += token
                    await websocket.send_text(json.dumps({"type": "note_delta", "text": token}))

                # Parse and persist complete note
                try:
                    # Strip markdown fences if present
                    clean = raw_json.strip()
                    if clean.startswith("```"):
                        clean = clean.split("```")[1]
                        if clean.startswith("json"):
                            clean = clean[4:]
                    note = json.loads(clean)
                except json.JSONDecodeError as exc:
                    await websocket.send_text(
                        json.dumps({"type": "error", "message": f"Note parse error: {exc}"})
                    )
                    note = {"raw": raw_json}

                async with AsyncSessionLocal() as db:
                    visit = await db.get(Visit, visit_id)
                    visit.note = note
                    visit.status = VisitStatus.complete
                    await db.commit()

                await websocket.send_text(json.dumps({"type": "note_complete", "note": note}))
                break

            # Binary audio chunk
            elif message.get("bytes"):
                buf = _audio_buffers.setdefault(visit_id, bytearray())
                buf.extend(message["bytes"])

                # Flush to ASR whenever buffer exceeds threshold
                if len(buf) >= settings.asr_buffer_bytes:
                    await _flush_buffer(visit_id, websocket)

    except WebSocketDisconnect:
        pass
    finally:
        _audio_buffers.pop(visit_id, None)
        _transcripts.pop(visit_id, None)
