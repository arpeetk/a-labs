import json

import anthropic

from ..config import settings

_client: anthropic.AsyncAnthropic | None = None

SYSTEM_PROMPT = """You are a clinical documentation AI. Given a doctor-patient conversation transcript,
produce a structured SOAP note. Every claim in the note MUST be grounded to a verbatim quote
from the transcript (linked evidence).

Return ONLY valid JSON matching this schema exactly:
{
  "subjective": {
    "chief_complaint": {"text": "...", "evidence": "exact quote from transcript"},
    "history_of_present_illness": {"text": "...", "evidence": "exact quote from transcript"},
    "review_of_systems": {"text": "...", "evidence": "exact quote from transcript"}
  },
  "objective": {
    "vitals": {"text": "...", "evidence": "exact quote from transcript or null if not mentioned"},
    "physical_exam": {"text": "...", "evidence": "exact quote from transcript or null if not mentioned"}
  },
  "assessment": {
    "diagnosis": {"text": "...", "evidence": "exact quote from transcript"},
    "differential": {"text": "...", "evidence": "exact quote from transcript or null"}
  },
  "plan": {
    "treatment": {"text": "...", "evidence": "exact quote from transcript"},
    "follow_up": {"text": "...", "evidence": "exact quote from transcript or null"},
    "patient_instructions": {"text": "...", "evidence": "exact quote from transcript or null"}
  },
  "hallucination_check": "none | low | medium | high"
}

Rules:
- evidence fields must be EXACT substrings from the transcript, or null if genuinely not mentioned
- Do not infer information not present in the transcript
- Set hallucination_check to "high" if you had to fill any critical field with inference
- If a section has no information in the transcript, set text to null and evidence to null
"""


def _get_client() -> anthropic.AsyncAnthropic:
    global _client
    if _client is None:
        _client = anthropic.AsyncAnthropic(api_key=settings.anthropic_api_key)
    return _client


async def generate_soap_note(transcript: str) -> dict:
    """Generate a structured SOAP note with linked evidence from a transcript."""
    client = _get_client()

    message = await client.messages.create(
        model="claude-sonnet-4-6",
        max_tokens=4096,
        system=SYSTEM_PROMPT,
        messages=[
            {
                "role": "user",
                "content": f"Here is the clinical encounter transcript:\n\n{transcript}\n\nGenerate the SOAP note.",
            }
        ],
    )

    raw = message.content[0].text.strip()
    # Strip markdown code fences if present
    if raw.startswith("```"):
        raw = raw.split("```")[1]
        if raw.startswith("json"):
            raw = raw[4:]
    return json.loads(raw)


async def stream_soap_note(transcript: str):
    """Stream SOAP note generation token-by-token for real-time display."""
    client = _get_client()

    async with client.messages.stream(
        model="claude-sonnet-4-6",
        max_tokens=4096,
        system=SYSTEM_PROMPT,
        messages=[
            {
                "role": "user",
                "content": f"Here is the clinical encounter transcript:\n\n{transcript}\n\nGenerate the SOAP note.",
            }
        ],
    ) as stream:
        async for text in stream.text_stream:
            yield text
