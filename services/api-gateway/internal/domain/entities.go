package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The types below mirror the rows of the system of record. Every timestamp is
// UTC. The `db` tags name the column each field is scanned from; they are
// the only coupling between these types and the schema.

// Role of a user.
type Role string

const (
	RoleAdmin    Role = "ADMIN"
	RoleOperator Role = "OPERATOR"
	RoleUser     Role = "USER"
)

// UserStatus of an account.
type UserStatus string

const (
	UserActive   UserStatus = "ACTIVE"
	UserDisabled UserStatus = "DISABLED"
)

// User is an account that authenticates against the gateway.
type User struct {
	ID           uuid.UUID  `db:"id"`
	Email        string     `db:"email"`
	PasswordHash string     `db:"password_hash"`
	Role         Role       `db:"role"`
	Status       UserStatus `db:"status"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`
}

// Sex recorded for a synthetic patient.
type Sex string

const (
	SexFemale  Sex = "FEMALE"
	SexMale    Sex = "MALE"
	SexOther   Sex = "OTHER"
	SexUnknown Sex = "UNKNOWN"
)

// PatientStatus of a patient record.
type PatientStatus string

const (
	PatientActive   PatientStatus = "ACTIVE"
	PatientInactive PatientStatus = "INACTIVE"
	PatientDeleted  PatientStatus = "DELETED"
)

// Patient is a synthetic patient.
type Patient struct {
	ID                uuid.UUID     `db:"id"`
	ExternalReference string        `db:"external_reference"`
	DateOfBirth       time.Time     `db:"date_of_birth"`
	Sex               Sex           `db:"sex"`
	Status            PatientStatus `db:"status"`
	CreatedAt         time.Time     `db:"created_at"`
	UpdatedAt         time.Time     `db:"updated_at"`
	DeletedAt         *time.Time    `db:"deleted_at"`
}

// MeasurementType is one of the accepted kinds of reading.
type MeasurementType string

const (
	HeartRate              MeasurementType = "HEART_RATE"
	BloodPressureSystolic  MeasurementType = "BLOOD_PRESSURE_SYSTOLIC"
	BloodPressureDiastolic MeasurementType = "BLOOD_PRESSURE_DIASTOLIC"
	SpO2                   MeasurementType = "SPO2"
	BodyTemperature        MeasurementType = "BODY_TEMPERATURE"
	BloodGlucose           MeasurementType = "BLOOD_GLUCOSE"
	RespiratoryRate        MeasurementType = "RESPIRATORY_RATE"
)

// Measurement is one synthetic reading.
type Measurement struct {
	ID         uuid.UUID       `db:"id"`
	PatientID  uuid.UUID       `db:"patient_id"`
	Type       MeasurementType `db:"type"`
	Value      float64         `db:"value"`
	Unit       string          `db:"unit"`
	RecordedAt time.Time       `db:"recorded_at"`
	CreatedAt  time.Time       `db:"created_at"`
	Source     string          `db:"source"`
	Metadata   json.RawMessage `db:"metadata"`
}

// JobStatus is the lifecycle state of a processing job.
type JobStatus string

const (
	JobPending    JobStatus = "PENDING"
	JobProcessing JobStatus = "PROCESSING"
	JobCompleted  JobStatus = "COMPLETED"
	JobFailed     JobStatus = "FAILED"
	JobCancelled  JobStatus = "CANCELLED"
)

// ProcessingJob is a request to process a patient's measurements.
type ProcessingJob struct {
	ID               uuid.UUID       `db:"id"`
	PatientID        uuid.UUID       `db:"patient_id"`
	Status           JobStatus       `db:"status"`
	Parameters       json.RawMessage `db:"parameters"`
	AlgorithmVersion string          `db:"algorithm_version"`
	RequestedAt      time.Time       `db:"requested_at"`
	StartedAt        *time.Time      `db:"started_at"`
	CompletedAt      *time.Time      `db:"completed_at"`
	FailedAt         *time.Time      `db:"failed_at"`
	CancelledAt      *time.Time      `db:"cancelled_at"`
	ErrorCode        *string         `db:"error_code"`
	ErrorMessage     *string         `db:"error_message"`
	AttemptCount     int             `db:"attempt_count"`
	CreatedBy        uuid.UUID       `db:"created_by"`
	ServiceVersion   *string         `db:"service_version"`
	RequestID        *string         `db:"request_id"`
	TraceID          *string         `db:"trace_id"`
	LeaseExpiresAt   *time.Time      `db:"lease_expires_at"`
	CreatedAt        time.Time       `db:"created_at"`
	UpdatedAt        time.Time       `db:"updated_at"`
}

// ProcessingResult is the outcome for one job, measurement type and window.
// PatientID is copied from the job by the database and is read-only.
type ProcessingResult struct {
	ID               uuid.UUID       `db:"id"`
	JobID            uuid.UUID       `db:"job_id"`
	PatientID        uuid.UUID       `db:"patient_id"`
	MeasurementType  MeasurementType `db:"measurement_type"`
	Window           string          `db:"window"`
	WindowStart      time.Time       `db:"window_start"`
	Statistics       json.RawMessage `db:"statistics"`
	Anomalies        json.RawMessage `db:"anomalies"`
	AlgorithmVersion string          `db:"algorithm_version"`
	ServiceVersion   string          `db:"service_version"`
	CreatedAt        time.Time       `db:"created_at"`
}

// ActorType says who performed an audited action.
type ActorType string

const (
	ActorUser   ActorType = "USER"
	ActorSystem ActorType = "SYSTEM"
)

// AuditAction is an audited operation.
type AuditAction string

const (
	AuditLogin                  AuditAction = "LOGIN"
	AuditLogout                 AuditAction = "LOGOUT"
	AuditUserCreated            AuditAction = "USER_CREATED"
	AuditRoleChanged            AuditAction = "ROLE_CHANGED"
	AuditPatientCreated         AuditAction = "PATIENT_CREATED"
	AuditPatientDeleted         AuditAction = "PATIENT_DELETED"
	AuditMeasurementCreated     AuditAction = "MEASUREMENT_CREATED"
	AuditMeasurementDeleted     AuditAction = "MEASUREMENT_DELETED"
	AuditProcessingJobCreated   AuditAction = "PROCESSING_JOB_CREATED"
	AuditProcessingJobCancelled AuditAction = "PROCESSING_JOB_CANCELLED"
	AuditRetentionRun           AuditAction = "RETENTION_RUN"
)

// AuditEntry is one append-only audit record.
type AuditEntry struct {
	ID           uuid.UUID       `db:"id"`
	ActorID      *uuid.UUID      `db:"actor_id"`
	ActorType    ActorType       `db:"actor_type"`
	Action       AuditAction     `db:"action"`
	ResourceType string          `db:"resource_type"`
	ResourceID   *uuid.UUID      `db:"resource_id"`
	RequestID    string          `db:"request_id"`
	Metadata     json.RawMessage `db:"metadata"`
	CreatedAt    time.Time       `db:"created_at"`
}

// IdempotencyStatus of a recorded request.
type IdempotencyStatus string

const (
	IdempotencyInProgress IdempotencyStatus = "IN_PROGRESS"
	IdempotencyCompleted  IdempotencyStatus = "COMPLETED"
)

// IdempotencyRecord is the durable memory of an idempotent write request.
type IdempotencyRecord struct {
	ID                 uuid.UUID         `db:"id"`
	UserID             uuid.UUID         `db:"user_id"`
	Method             string            `db:"method"`
	Path               string            `db:"path"`
	Key                string            `db:"key"`
	RequestFingerprint string            `db:"request_fingerprint"`
	Status             IdempotencyStatus `db:"status"`
	ResponseStatus     *int              `db:"response_status"`
	// ResponseHeaders are the allow-listed headers of the stored response,
	// set again on a replay so that a replay is the same response and not
	// merely the same status and body.
	ResponseHeaders map[string]string `db:"response_headers"`
	ResponseBody    json.RawMessage   `db:"response_body"`
	CreatedAt       time.Time         `db:"created_at"`
	ExpiresAt       time.Time         `db:"expires_at"`
}
