from datetime import datetime
from typing import Any

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from ..database import get_db
from ..models import Visit, VisitStatus
from ..services import generate_soap_note, transcribe

router = APIRouter(prefix="/visits", tags=["visits"])


class CreateVisitRequest(BaseModel):
    patient_name: str
    clinician_name: str


class VisitResponse(BaseModel):
    id: int
    patient_name: str
    clinician_name: str
    status: str
    transcript: str | None
    note: Any | None
    created_at: datetime
    updated_at: datetime

    model_config = {"from_attributes": True}


@router.post("", response_model=VisitResponse, status_code=201)
async def create_visit(body: CreateVisitRequest, db: AsyncSession = Depends(get_db)):
    visit = Visit(patient_name=body.patient_name, clinician_name=body.clinician_name)
    db.add(visit)
    await db.commit()
    await db.refresh(visit)
    return visit


@router.get("", response_model=list[VisitResponse])
async def list_visits(db: AsyncSession = Depends(get_db)):
    result = await db.execute(select(Visit).order_by(Visit.created_at.desc()).limit(50))
    return result.scalars().all()


@router.get("/{visit_id}", response_model=VisitResponse)
async def get_visit(visit_id: int, db: AsyncSession = Depends(get_db)):
    visit = await db.get(Visit, visit_id)
    if not visit:
        raise HTTPException(status_code=404, detail="Visit not found")
    return visit


@router.post("/{visit_id}/finalize", response_model=VisitResponse)
async def finalize_visit(visit_id: int, db: AsyncSession = Depends(get_db)):
    """
    Called when the clinician ends the visit. Runs ASR on any buffered audio
    (already done during streaming) then generates the SOAP note via Claude.
    Expects transcript to already be populated by the WebSocket handler.
    """
    visit = await db.get(Visit, visit_id)
    if not visit:
        raise HTTPException(status_code=404, detail="Visit not found")
    if not visit.transcript:
        raise HTTPException(status_code=400, detail="No transcript available — was audio streamed?")

    visit.status = VisitStatus.generating
    await db.commit()

    try:
        note = await generate_soap_note(visit.transcript)
        visit.note = note
        visit.status = VisitStatus.complete
    except Exception as exc:
        visit.status = VisitStatus.error
        await db.commit()
        raise HTTPException(status_code=500, detail=str(exc)) from exc

    await db.commit()
    await db.refresh(visit)
    return visit
