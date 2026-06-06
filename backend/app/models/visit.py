from datetime import datetime
from enum import Enum

from sqlalchemy import JSON, Column, DateTime, Integer, String, Text
from sqlalchemy.orm import DeclarativeBase


class Base(DeclarativeBase):
    pass


class VisitStatus(str, Enum):
    recording = "recording"
    transcribing = "transcribing"
    generating = "generating"
    complete = "complete"
    error = "error"


class Visit(Base):
    __tablename__ = "visits"

    id = Column(Integer, primary_key=True, autoincrement=True)
    patient_name = Column(String(255), nullable=False)
    clinician_name = Column(String(255), nullable=False)
    status = Column(String(50), default=VisitStatus.recording)
    transcript = Column(Text, nullable=True)          # full assembled transcript
    note = Column(JSON, nullable=True)                # structured SOAP note with evidence
    created_at = Column(DateTime, default=datetime.utcnow)
    updated_at = Column(DateTime, default=datetime.utcnow, onupdate=datetime.utcnow)
