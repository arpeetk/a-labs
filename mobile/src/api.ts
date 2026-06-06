import { API_BASE_URL } from "./config";

export interface Visit {
  id: number;
  patient_name: string;
  clinician_name: string;
  status: "recording" | "transcribing" | "generating" | "complete" | "error";
  transcript: string | null;
  note: SoapNote | null;
  created_at: string;
  updated_at: string;
}

export interface NoteSection {
  text: string | null;
  evidence: string | null;
}

export interface SoapNote {
  subjective: {
    chief_complaint: NoteSection;
    history_of_present_illness: NoteSection;
    review_of_systems: NoteSection;
  };
  objective: {
    vitals: NoteSection;
    physical_exam: NoteSection;
  };
  assessment: {
    diagnosis: NoteSection;
    differential: NoteSection;
  };
  plan: {
    treatment: NoteSection;
    follow_up: NoteSection;
    patient_instructions: NoteSection;
  };
  hallucination_check: "none" | "low" | "medium" | "high";
}

export async function createVisit(patientName: string, clinicianName: string): Promise<Visit> {
  const res = await fetch(`${API_BASE_URL}/visits`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ patient_name: patientName, clinician_name: clinicianName }),
  });
  if (!res.ok) throw new Error(`Failed to create visit: ${res.status}`);
  return res.json();
}

export async function getVisit(id: number): Promise<Visit> {
  const res = await fetch(`${API_BASE_URL}/visits/${id}`);
  if (!res.ok) throw new Error(`Visit not found: ${res.status}`);
  return res.json();
}

export async function listVisits(): Promise<Visit[]> {
  const res = await fetch(`${API_BASE_URL}/visits`);
  if (!res.ok) throw new Error("Failed to fetch visits");
  return res.json();
}
