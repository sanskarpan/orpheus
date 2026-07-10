from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional

DESCRIPTOR: _descriptor.FileDescriptor

class JobStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    JOB_STATUS_UNSPECIFIED: _ClassVar[JobStatus]
    JOB_STATUS_QUEUED: _ClassVar[JobStatus]
    JOB_STATUS_RUNNING: _ClassVar[JobStatus]
    JOB_STATUS_COMPLETED: _ClassVar[JobStatus]
    JOB_STATUS_FAILED: _ClassVar[JobStatus]
    JOB_STATUS_CANCELED: _ClassVar[JobStatus]

class JobType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    JOB_TYPE_UNSPECIFIED: _ClassVar[JobType]
    JOB_TYPE_EXTRACT_METADATA: _ClassVar[JobType]
    JOB_TYPE_PROBE: _ClassVar[JobType]
    JOB_TYPE_SLICE: _ClassVar[JobType]
JOB_STATUS_UNSPECIFIED: JobStatus
JOB_STATUS_QUEUED: JobStatus
JOB_STATUS_RUNNING: JobStatus
JOB_STATUS_COMPLETED: JobStatus
JOB_STATUS_FAILED: JobStatus
JOB_STATUS_CANCELED: JobStatus
JOB_TYPE_UNSPECIFIED: JobType
JOB_TYPE_EXTRACT_METADATA: JobType
JOB_TYPE_PROBE: JobType
JOB_TYPE_SLICE: JobType

class Page(_message.Message):
    __slots__ = ("limit", "cursor")
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    CURSOR_FIELD_NUMBER: _ClassVar[int]
    limit: int
    cursor: str
    def __init__(self, limit: _Optional[int] = ..., cursor: _Optional[str] = ...) -> None: ...

class TenantRef(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...
