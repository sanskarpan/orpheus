import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from orpheus.v1 import common_pb2 as _common_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProcessorRef(_message.Message):
    __slots__ = ("name", "version")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: str
    def __init__(self, name: _Optional[str] = ..., version: _Optional[str] = ...) -> None: ...

class Job(_message.Message):
    __slots__ = ("id", "org", "artifact_id", "processor", "job_type", "status", "created_at", "updated_at", "started_at", "completed_at", "attempts", "max_retries")
    ID_FIELD_NUMBER: _ClassVar[int]
    ORG_FIELD_NUMBER: _ClassVar[int]
    ARTIFACT_ID_FIELD_NUMBER: _ClassVar[int]
    PROCESSOR_FIELD_NUMBER: _ClassVar[int]
    JOB_TYPE_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    UPDATED_AT_FIELD_NUMBER: _ClassVar[int]
    STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    COMPLETED_AT_FIELD_NUMBER: _ClassVar[int]
    ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    MAX_RETRIES_FIELD_NUMBER: _ClassVar[int]
    id: str
    org: _common_pb2.TenantRef
    artifact_id: str
    processor: ProcessorRef
    job_type: _common_pb2.JobType
    status: _common_pb2.JobStatus
    created_at: _timestamp_pb2.Timestamp
    updated_at: _timestamp_pb2.Timestamp
    started_at: _timestamp_pb2.Timestamp
    completed_at: _timestamp_pb2.Timestamp
    attempts: int
    max_retries: int
    def __init__(self, id: _Optional[str] = ..., org: _Optional[_Union[_common_pb2.TenantRef, _Mapping]] = ..., artifact_id: _Optional[str] = ..., processor: _Optional[_Union[ProcessorRef, _Mapping]] = ..., job_type: _Optional[_Union[_common_pb2.JobType, str]] = ..., status: _Optional[_Union[_common_pb2.JobStatus, str]] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., updated_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., started_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., completed_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., attempts: _Optional[int] = ..., max_retries: _Optional[int] = ...) -> None: ...

class PingRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class PingResponse(_message.Message):
    __slots__ = ("worker_version", "supported_job_types")
    WORKER_VERSION_FIELD_NUMBER: _ClassVar[int]
    SUPPORTED_JOB_TYPES_FIELD_NUMBER: _ClassVar[int]
    worker_version: str
    supported_job_types: _containers.RepeatedScalarFieldContainer[_common_pb2.JobType]
    def __init__(self, worker_version: _Optional[str] = ..., supported_job_types: _Optional[_Iterable[_Union[_common_pb2.JobType, str]]] = ...) -> None: ...

class GetJobStatusRequest(_message.Message):
    __slots__ = ("job_id",)
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    def __init__(self, job_id: _Optional[str] = ...) -> None: ...

class GetJobStatusResponse(_message.Message):
    __slots__ = ("job_id", "status", "status_detail", "updated_at")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    STATUS_DETAIL_FIELD_NUMBER: _ClassVar[int]
    UPDATED_AT_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    status: _common_pb2.JobStatus
    status_detail: str
    updated_at: _timestamp_pb2.Timestamp
    def __init__(self, job_id: _Optional[str] = ..., status: _Optional[_Union[_common_pb2.JobStatus, str]] = ..., status_detail: _Optional[str] = ..., updated_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
