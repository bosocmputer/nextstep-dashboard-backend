alter table tenant_sml_connections
  add column config_file_name text not null default 'SMLConfigDATA.xml';
