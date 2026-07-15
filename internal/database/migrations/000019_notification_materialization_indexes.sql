-- nextstep:no-transaction
create unique index concurrently notification_run_reports_position_unique_idx
  on notification_run_reports (notification_run_id, position)
  where position is not null;
