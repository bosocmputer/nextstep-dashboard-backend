create table admin_users (
  username text primary key check (char_length(username) between 1 and 120),
  password_hash text not null,
  must_rotate_password boolean not null default true,
  password_updated_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

alter table admin_sessions
  add constraint admin_sessions_username_fk
  foreign key (username) references admin_users(username) on delete cascade;
