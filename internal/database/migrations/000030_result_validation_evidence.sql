alter table report_runs
  drop constraint report_runs_failure_evidence_version_check,
  drop constraint report_runs_failure_protocol_evidence_check,
  drop constraint report_runs_failure_evidence_shape_check;

alter table report_runs
  add constraint report_runs_failure_evidence_version_check
    check (failure_evidence_version is null or failure_evidence_version in (1, 2, 3)) not valid,
  add constraint report_runs_failure_protocol_evidence_check
    check (
      failure_protocol_evidence is null
      or (
        failure_evidence_version in (2, 3)
        and jsonb_typeof(failure_protocol_evidence) = 'object'
        and octet_length(failure_protocol_evidence::text) <= 4096
        and coalesce(failure_protocol_evidence->>'requestRef', '') ~ '^NXR-[A-Z2-7]{16}$'
        and coalesce((failure_protocol_evidence->>'requestCount')::integer, 0) between 0 and 100
        and coalesce((failure_protocol_evidence->>'retryCount')::integer, 0) between 0 and 99
        and coalesce((failure_protocol_evidence->>'tenantConcurrentQueries')::integer, 0) between 0 and 100
        and coalesce((failure_protocol_evidence->>'hostConcurrentQueries')::integer, 0) between 0 and 100
        and (not (failure_protocol_evidence ? 'responseSha256') or failure_protocol_evidence->>'responseSha256' ~ '^[0-9a-f]{64}$')
        and (not (failure_protocol_evidence ? 'resultXmlBytes') or (failure_protocol_evidence->>'resultXmlBytes')::bigint between 0 and 1073741824)
        and (not (failure_protocol_evidence ? 'resultValidationOffsetBytes') or (failure_protocol_evidence->>'resultValidationOffsetBytes')::bigint between 0 and 1073741824)
        and (not (failure_protocol_evidence ? 'resultRowsDecoded') or (failure_protocol_evidence->>'resultRowsDecoded')::integer between 0 and 1000000)
        and (not (failure_protocol_evidence ? 'resultSetSeen') or jsonb_typeof(failure_protocol_evidence->'resultSetSeen') = 'boolean')
        and (
          not (failure_protocol_evidence ? 'resultValidationCode')
          or failure_protocol_evidence->>'resultValidationCode' in (
            'PARSER_CONFIGURATION_INVALID', 'XML_MALFORMED', 'RESULT_SET_MISSING',
            'ROW_LIMIT_EXCEEDED', 'ROW_MALFORMED', 'FIELD_MALFORMED', 'FIELD_VALUE_TOO_LARGE'
          )
        )
        and (
          failure_evidence_version <> 3
          or (
            failure_protocol_evidence ? 'resultValidationCode'
            and failure_protocol_evidence ? 'resultXmlBytes'
            and failure_protocol_evidence ? 'resultValidationOffsetBytes'
            and failure_protocol_evidence ? 'resultRowsDecoded'
            and failure_protocol_evidence ? 'resultSetSeen'
          )
        )
      )
    ) not valid,
  add constraint report_runs_failure_evidence_shape_check
    check (
      (failure_evidence_version is null and failure_category is null and failure_stage is null and failure_protocol_evidence is null)
      or (failure_evidence_version in (1, 2, 3) and failure_category is not null and failure_stage is not null
          and failure_occurred_at is not null and failure_retryable is not null
          and failure_remote_state_unknown is not null
          and (failure_evidence_version = 1 or failure_protocol_evidence is not null))
    ) not valid;

alter table report_runs validate constraint report_runs_failure_evidence_version_check;
alter table report_runs validate constraint report_runs_failure_protocol_evidence_check;
alter table report_runs validate constraint report_runs_failure_evidence_shape_check;

alter table operational_incident_events
  drop constraint operational_incident_events_failure_version_check,
  drop constraint operational_incident_events_failure_protocol_evidence_check;

alter table operational_incident_events
  add constraint operational_incident_events_failure_version_check
    check (failure_evidence_version is null or failure_evidence_version in (1, 2, 3)) not valid,
  add constraint operational_incident_events_failure_protocol_evidence_check
    check (
      failure_protocol_evidence is null
      or (
        failure_evidence_version in (2, 3)
        and jsonb_typeof(failure_protocol_evidence) = 'object'
        and octet_length(failure_protocol_evidence::text) <= 4096
        and coalesce(failure_protocol_evidence->>'requestRef', '') ~ '^NXR-[A-Z2-7]{16}$'
        and (not (failure_protocol_evidence ? 'responseSha256') or failure_protocol_evidence->>'responseSha256' ~ '^[0-9a-f]{64}$')
        and (not (failure_protocol_evidence ? 'resultXmlBytes') or (failure_protocol_evidence->>'resultXmlBytes')::bigint between 0 and 1073741824)
        and (not (failure_protocol_evidence ? 'resultValidationOffsetBytes') or (failure_protocol_evidence->>'resultValidationOffsetBytes')::bigint between 0 and 1073741824)
        and (not (failure_protocol_evidence ? 'resultRowsDecoded') or (failure_protocol_evidence->>'resultRowsDecoded')::integer between 0 and 1000000)
        and (not (failure_protocol_evidence ? 'resultSetSeen') or jsonb_typeof(failure_protocol_evidence->'resultSetSeen') = 'boolean')
        and (
          not (failure_protocol_evidence ? 'resultValidationCode')
          or failure_protocol_evidence->>'resultValidationCode' in (
            'PARSER_CONFIGURATION_INVALID', 'XML_MALFORMED', 'RESULT_SET_MISSING',
            'ROW_LIMIT_EXCEEDED', 'ROW_MALFORMED', 'FIELD_MALFORMED', 'FIELD_VALUE_TOO_LARGE'
          )
        )
        and (
          failure_evidence_version <> 3
          or (
            failure_protocol_evidence ? 'resultValidationCode'
            and failure_protocol_evidence ? 'resultXmlBytes'
            and failure_protocol_evidence ? 'resultValidationOffsetBytes'
            and failure_protocol_evidence ? 'resultRowsDecoded'
            and failure_protocol_evidence ? 'resultSetSeen'
          )
        )
      )
    ) not valid;

alter table operational_incident_events validate constraint operational_incident_events_failure_version_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_protocol_evidence_check;
