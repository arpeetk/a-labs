import io
import wave

from openai import AsyncOpenAI

from ..config import settings

_client: AsyncOpenAI | None = None


def _get_client() -> AsyncOpenAI:
    global _client
    if _client is None:
        _client = AsyncOpenAI(api_key=settings.openai_api_key)
    return _client


async def transcribe(pcm_bytes: bytes, sample_rate: int = 48000) -> str:
    """Transcribe raw 16-bit mono PCM audio via Whisper."""
    wav_buf = io.BytesIO()
    with wave.open(wav_buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)   # 16-bit
        wf.setframerate(sample_rate)
        wf.writeframes(pcm_bytes)
    wav_buf.seek(0)
    wav_buf.name = "audio.wav"

    client = _get_client()
    result = await client.audio.transcriptions.create(
        model="whisper-1",
        file=wav_buf,
        response_format="verbose_json",  # includes word-level timestamps
        timestamp_granularities=["segment"],
    )
    return result.text
