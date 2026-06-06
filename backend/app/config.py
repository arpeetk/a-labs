from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8")

    openai_api_key: str = ""
    anthropic_api_key: str = ""
    database_url: str = "sqlite+aiosqlite:///./abridge.db"
    # Audio chunk size sent by mobile (bytes). Whisper needs at least a few
    # seconds of audio for reliable transcription, so we buffer until this
    # threshold before flushing to ASR.
    asr_buffer_bytes: int = 48000 * 2 * 3  # 3 s @ 48 kHz 16-bit mono


settings = Settings()
