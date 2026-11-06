-- GPU.ai Open Source: initial schema.
--
-- A consolidated baseline of the tables the open-source server uses, derived
-- from GPU.ai's production schema with billing, inference, community,
-- marketplace and dashboard-only tables removed. Later schema changes ship as
-- new, higher-numbered files in this directory; never edit an applied file.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE OR REPLACE FUNCTION update_updated_at()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$function$;

--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE api_keys (
    api_key_id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    user_id uuid NOT NULL,
    name character varying(128) NOT NULL,
    key_hash character(64) NOT NULL,
    key_prefix character varying(24) NOT NULL,
    last4 character(4) NOT NULL,
    scopes jsonb NOT NULL,
    rate_limit_rps integer DEFAULT 100 NOT NULL,
    rate_limit_burst integer DEFAULT 200 NOT NULL,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    expires_at timestamp with time zone
);

--
-- Name: audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE audit_log (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    user_id uuid,
    action character varying(100) NOT NULL,
    resource_type character varying(50),
    resource_id character varying(255),
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now()
);

--
-- Name: frp_remote_port_cursor; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE frp_remote_port_cursor
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: gpu_telemetry; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE gpu_telemetry (
    id bigint NOT NULL,
    instance_id text NOT NULL,
    ts timestamp with time zone DEFAULT now() NOT NULL,
    sm_util integer,
    vram_used_mb integer,
    vram_total_mb integer,
    power_w integer
);

--
-- Name: gpu_telemetry_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE gpu_telemetry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: gpu_telemetry_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE gpu_telemetry_id_seq OWNED BY gpu_telemetry.id;

--
-- Name: instance_bootstraps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE instance_bootstraps (
    instance_id text NOT NULL,
    script text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: instance_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE instance_events (
    event_id uuid DEFAULT gen_random_uuid() NOT NULL,
    instance_id character varying(32) NOT NULL,
    org_id uuid NOT NULL,
    event_type character varying(50) NOT NULL,
    metadata jsonb,
    created_at timestamp with time zone DEFAULT now(),
    CONSTRAINT instance_events_event_type_check CHECK (event_type IN ('ready', 'interrupted', 'failed', 'terminated', 'unreachable', 'rescued', 'preemption_warned', 'preemption_reclaimed', 'auto_terminated'))
);

--
-- Name: instance_ready_reports; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE instance_ready_reports (
    report_id bigint NOT NULL,
    instance_id text NOT NULL,
    provider text NOT NULL,
    upstream_id text NOT NULL,
    report_hash text NOT NULL,
    report jsonb NOT NULL,
    first_observed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    last_observed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    callbacks_received bigint DEFAULT 1 NOT NULL,
    CONSTRAINT instance_ready_reports_callbacks_received_check CHECK ((callbacks_received > 0)),
    CONSTRAINT instance_ready_reports_report_check CHECK (((jsonb_typeof(report) = 'object'::text) AND (octet_length((report)::text) <= 2048))),
    CONSTRAINT instance_ready_reports_report_hash_check CHECK ((report_hash ~ '^[0-9a-f]{64}$'::text))
);

--
-- Name: instance_ready_reports_report_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE instance_ready_reports_report_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: instance_ready_reports_report_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE instance_ready_reports_report_id_seq OWNED BY instance_ready_reports.report_id;

--
-- Name: instances; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE instances (
    instance_id character varying(12) NOT NULL,
    org_id uuid NOT NULL,
    user_id uuid NOT NULL,
    upstream_provider character varying(50) NOT NULL,
    upstream_id character varying(255) NOT NULL,
    upstream_ip inet,
    hostname character varying(255) NOT NULL,
    wg_public_key character varying(255),
    wg_address inet,
    gpu_type character varying(50) NOT NULL,
    gpu_count integer NOT NULL,
    tier character varying(20) NOT NULL,
    region character varying(50) NOT NULL,
    price_per_hour numeric(10,4) NOT NULL,
    upstream_price_per_hour numeric(10,4) NOT NULL,
    billing_start timestamp with time zone,
    billing_end timestamp with time zone,
    status character varying(20) DEFAULT 'creating'::character varying,
    created_at timestamp with time zone DEFAULT now(),
    terminated_at timestamp with time zone,
    internal_token character varying(255),
    updated_at timestamp with time zone DEFAULT now(),
    wg_private_key_enc text,
    name character varying(255),
    ready_at timestamp with time zone,
    error_reason text,
    frp_remote_port integer,
    ssh_port integer,
    reservation_capacity_id uuid,
    cleanup_status character varying(20),
    template_id text,
    admin_terminating_at timestamp with time zone,
    disk_gb integer,
    app_auth_user text,
    app_auth_pass text,
    terminal_port integer,
    last_reachable_at timestamp with time zone,
    unreachable_since timestamp with time zone,
    image text,
    app_port integer,
    auto_terminate_hours integer,
    auto_terminate_at timestamp with time zone,
    readiness_failure text,
    readiness_failed_at timestamp with time zone,
    CONSTRAINT instances_auto_terminate_hours_check CHECK (((auto_terminate_hours IS NULL) OR ((auto_terminate_hours >= 1) AND (auto_terminate_hours <= 720)))),
    CONSTRAINT instances_cleanup_status_check CHECK (((cleanup_status)::text = ANY ((ARRAY['done'::character varying, 'pending'::character varying])::text[]))),
    CONSTRAINT instances_status_check CHECK (((status)::text = ANY ((ARRAY['creating'::character varying, 'provisioning'::character varying, 'booting'::character varying, 'running'::character varying, 'stopping'::character varying, 'stopped'::character varying, 'terminated'::character varying, 'error'::character varying])::text[])))
);

--
-- Name: COLUMN instances.admin_terminating_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN instances.admin_terminating_at IS 'operator force-terminate marker; non-NULL = reconciler skips this row';

--
-- Name: operations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE operations (
    operation_id uuid DEFAULT gen_random_uuid() NOT NULL,
    api_key_id uuid,
    org_id uuid NOT NULL,
    kind character varying(32) NOT NULL,
    state character varying(16) NOT NULL,
    resource_id character varying(64),
    error_code character varying(64),
    error_detail text,
    metadata jsonb,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    result jsonb,
    CONSTRAINT operations_state_check CHECK (((state)::text = ANY ((ARRAY['pending'::character varying, 'in_progress'::character varying, 'succeeded'::character varying, 'failed'::character varying, 'cancelled'::character varying])::text[])))
);

--
-- Name: COLUMN operations.api_key_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN operations.api_key_id IS 'The API key that issued this operation. GetOperation is issuer-scoped on this column, so an operation is visible only to the key that created it.';

--
-- Name: organizations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE organizations (
    organization_id uuid DEFAULT gen_random_uuid() NOT NULL,
    name character varying(255) NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    external_org_id character varying(255)
);

--
-- Name: provision_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE provision_attempts (
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    instance_id text,
    org_id text,
    attempt_seq integer NOT NULL,
    phase text NOT NULL,
    outcome text NOT NULL,
    provider text,
    gpu_type text NOT NULL,
    gpu_count integer DEFAULT 1 NOT NULL,
    region text,
    capacity_class text,
    candidates_matched integer,
    attempts_allowed integer,
    rescues_remaining integer,
    budget_remaining_ms bigint,
    duration_ms bigint,
    reason text
);

--
-- Name: provision_failure_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE provision_failure_events (
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    gpu_type text NOT NULL,
    gpu_count integer DEFAULT 1 NOT NULL,
    org_id text,
    instance_id text,
    stock_related boolean NOT NULL,
    reason text NOT NULL
);

--
-- Name: public_api_idempotency_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public_api_idempotency_keys (
    api_key_id uuid NOT NULL,
    idempotency_key character varying(255) NOT NULL,
    request_hash character(64) NOT NULL,
    response_code integer,
    response_body bytea,
    response_headers jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: registry_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE registry_credentials (
    credential_id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id text NOT NULL,
    name text NOT NULL,
    registry text NOT NULL,
    username text NOT NULL,
    password_enc bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: ssh_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE ssh_keys (
    ssh_key_id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid,
    name character varying(255),
    public_key text NOT NULL,
    fingerprint character varying(255),
    created_at timestamp with time zone DEFAULT now(),
    org_id uuid NOT NULL,
    attach_on_provision boolean DEFAULT true NOT NULL
);

--
-- Name: suppliers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE suppliers (
    supplier_id uuid DEFAULT gen_random_uuid() NOT NULL,
    slug character varying(63) NOT NULL,
    name character varying(255) NOT NULL,
    base_url character varying(512) NOT NULL,
    client_id character varying(255) NOT NULL,
    client_secret_hash character varying(255) NOT NULL,
    status character varying(20) DEFAULT 'active'::character varying NOT NULL,
    regions text[] DEFAULT '{}'::text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT suppliers_status_check CHECK (((status)::text = ANY ((ARRAY['active'::character varying, 'suspended'::character varying])::text[])))
);

--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE users (
    user_id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    external_user_id character varying(255),
    email character varying(255),
    name character varying(255),
    role character varying(50) DEFAULT 'member'::character varying,
    created_at timestamp with time zone DEFAULT now(),
    image_url character varying(512),
    onboarded_at timestamp with time zone,
    last_sign_in_at timestamp with time zone,
    gclid text,
    gclid_captured_at timestamp with time zone,
    CONSTRAINT users_role_check CHECK (((role)::text = ANY ((ARRAY['owner'::character varying, 'admin'::character varying, 'member'::character varying])::text[])))
);

--
-- Name: gpu_telemetry id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY gpu_telemetry ALTER COLUMN id SET DEFAULT nextval('gpu_telemetry_id_seq'::regclass);

--
-- Name: instance_ready_reports report_id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_ready_reports ALTER COLUMN report_id SET DEFAULT nextval('instance_ready_reports_report_id_seq'::regclass);

--
-- Name: api_keys api_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY api_keys
    ADD CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash);

--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (api_key_id);

--
-- Name: audit_log audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);

--
-- Name: gpu_telemetry gpu_telemetry_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY gpu_telemetry
    ADD CONSTRAINT gpu_telemetry_pkey PRIMARY KEY (id);

--
-- Name: instance_bootstraps instance_bootstraps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_bootstraps
    ADD CONSTRAINT instance_bootstraps_pkey PRIMARY KEY (instance_id);

--
-- Name: instance_events instance_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_events
    ADD CONSTRAINT instance_events_pkey PRIMARY KEY (event_id);

--
-- Name: instance_ready_reports instance_ready_reports_instance_id_report_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_ready_reports
    ADD CONSTRAINT instance_ready_reports_instance_id_report_hash_key UNIQUE (instance_id, report_hash);

--
-- Name: instance_ready_reports instance_ready_reports_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_ready_reports
    ADD CONSTRAINT instance_ready_reports_pkey PRIMARY KEY (report_id);

--
-- Name: instances instances_hostname_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instances
    ADD CONSTRAINT instances_hostname_unique UNIQUE (hostname);

--
-- Name: instances instances_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instances
    ADD CONSTRAINT instances_pkey PRIMARY KEY (instance_id);

--
-- Name: instances instances_wg_address_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instances
    ADD CONSTRAINT instances_wg_address_unique UNIQUE (wg_address);

--
-- Name: operations operations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY operations
    ADD CONSTRAINT operations_pkey PRIMARY KEY (operation_id);

--
-- Name: organizations organizations_external_org_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY organizations
    ADD CONSTRAINT organizations_external_org_id_key UNIQUE (external_org_id);

--
-- Name: organizations organizations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY organizations
    ADD CONSTRAINT organizations_pkey PRIMARY KEY (organization_id);

--
-- Name: public_api_idempotency_keys public_api_idempotency_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public_api_idempotency_keys
    ADD CONSTRAINT public_api_idempotency_keys_pkey PRIMARY KEY (api_key_id, idempotency_key);

--
-- Name: registry_credentials registry_credentials_org_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY registry_credentials
    ADD CONSTRAINT registry_credentials_org_id_name_key UNIQUE (org_id, name);

--
-- Name: registry_credentials registry_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY registry_credentials
    ADD CONSTRAINT registry_credentials_pkey PRIMARY KEY (credential_id);

--
-- Name: ssh_keys ssh_keys_org_fingerprint_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY ssh_keys
    ADD CONSTRAINT ssh_keys_org_fingerprint_unique UNIQUE (org_id, fingerprint);

--
-- Name: ssh_keys ssh_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY ssh_keys
    ADD CONSTRAINT ssh_keys_pkey PRIMARY KEY (ssh_key_id);

--
-- Name: suppliers suppliers_client_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY suppliers
    ADD CONSTRAINT suppliers_client_id_key UNIQUE (client_id);

--
-- Name: suppliers suppliers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY suppliers
    ADD CONSTRAINT suppliers_pkey PRIMARY KEY (supplier_id);

--
-- Name: suppliers suppliers_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY suppliers
    ADD CONSTRAINT suppliers_slug_key UNIQUE (slug);

--
-- Name: users users_external_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY users
    ADD CONSTRAINT users_external_user_id_key UNIQUE (external_user_id);

--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY users
    ADD CONSTRAINT users_pkey PRIMARY KEY (user_id);

--
-- Name: idx_api_keys_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_hash ON api_keys USING btree (key_hash) WHERE (revoked_at IS NULL);

--
-- Name: idx_api_keys_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_org ON api_keys USING btree (org_id) WHERE (revoked_at IS NULL);

--
-- Name: idx_audit_log_org_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_log_org_created ON audit_log USING btree (org_id, created_at DESC);

--
-- Name: idx_audit_log_resource; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_audit_log_resource ON audit_log USING btree (resource_type, resource_id, created_at DESC);

--
-- Name: idx_gpu_telemetry_instance_ts; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_gpu_telemetry_instance_ts ON gpu_telemetry USING btree (instance_id, ts DESC);

--
-- Name: idx_instance_events_instance; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instance_events_instance ON instance_events USING btree (instance_id);

--
-- Name: idx_instance_events_org_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instance_events_org_created ON instance_events USING btree (org_id, created_at DESC);

--
-- Name: idx_instance_ready_reports_recent; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instance_ready_reports_recent ON instance_ready_reports USING btree (instance_id, last_observed_at DESC, report_id DESC);

--
-- Name: idx_instances_admin_terminating; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_admin_terminating ON instances USING btree (admin_terminating_at) WHERE (admin_terminating_at IS NOT NULL);

--
-- Name: idx_instances_auto_terminate_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_auto_terminate_due ON instances USING btree (auto_terminate_at) WHERE (auto_terminate_at IS NOT NULL);

--
-- Name: idx_instances_created_at_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_created_at_id ON instances USING btree (created_at DESC, instance_id DESC);

--
-- Name: idx_instances_org_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_org_id ON instances USING btree (org_id);

--
-- Name: idx_instances_reservation_capacity; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_reservation_capacity ON instances USING btree (reservation_capacity_id) WHERE (reservation_capacity_id IS NOT NULL);

--
-- Name: idx_instances_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_status ON instances USING btree (status);

--
-- Name: idx_instances_upstream_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_instances_upstream_unique ON instances USING btree (upstream_provider, upstream_id);

--
-- Name: idx_instances_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_instances_user_id ON instances USING btree (user_id);

--
-- Name: idx_operations_org_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_operations_org_created ON operations USING btree (org_id, created_at DESC);

--
-- Name: idx_operations_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_operations_pending ON operations USING btree (created_at) WHERE ((state)::text = ANY ((ARRAY['pending'::character varying, 'in_progress'::character varying])::text[]));

--
-- Name: idx_operations_resource; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_operations_resource ON operations USING btree (resource_id) WHERE (resource_id IS NOT NULL);

--
-- Name: idx_provision_attempts_instance_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_provision_attempts_instance_id ON provision_attempts USING btree (instance_id);

--
-- Name: idx_provision_attempts_occurred_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_provision_attempts_occurred_at ON provision_attempts USING btree (occurred_at);

--
-- Name: idx_provision_failure_events_occurred_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_provision_failure_events_occurred_at ON provision_failure_events USING btree (occurred_at);

--
-- Name: idx_public_api_idempotency_cleanup; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_public_api_idempotency_cleanup ON public_api_idempotency_keys USING btree (created_at);

--
-- Name: idx_registry_credentials_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_registry_credentials_org ON registry_credentials USING btree (org_id);

--
-- Name: idx_ssh_keys_org_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ssh_keys_org_id ON ssh_keys USING btree (org_id);

--
-- Name: idx_ssh_keys_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ssh_keys_user_id ON ssh_keys USING btree (user_id);

--
-- Name: idx_suppliers_client_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_suppliers_client_id ON suppliers USING btree (client_id);

--
-- Name: idx_suppliers_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_suppliers_status ON suppliers USING btree (status) WHERE ((status)::text = 'active'::text);

--
-- Name: idx_users_org_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_org_id ON users USING btree (org_id);

--
-- Name: instances_frp_remote_port_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX instances_frp_remote_port_active ON instances USING btree (frp_remote_port) WHERE ((frp_remote_port IS NOT NULL) AND ((status)::text <> ALL ((ARRAY['terminated'::character varying, 'error'::character varying])::text[])));

--
-- Name: instances trg_instances_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_instances_updated_at BEFORE UPDATE ON instances FOR EACH ROW EXECUTE FUNCTION update_updated_at();

--
-- Name: api_keys api_keys_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY api_keys
    ADD CONSTRAINT api_keys_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE CASCADE;

--
-- Name: api_keys api_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY api_keys
    ADD CONSTRAINT api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE CASCADE;

--
-- Name: audit_log audit_log_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY audit_log
    ADD CONSTRAINT audit_log_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE RESTRICT;

--
-- Name: audit_log audit_log_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY audit_log
    ADD CONSTRAINT audit_log_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(user_id);

--
-- Name: instance_events instance_events_instance_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_events
    ADD CONSTRAINT instance_events_instance_id_fkey FOREIGN KEY (instance_id) REFERENCES instances(instance_id) ON DELETE RESTRICT;

--
-- Name: instance_events instance_events_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_events
    ADD CONSTRAINT instance_events_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE RESTRICT;

--
-- Name: instance_ready_reports instance_ready_reports_instance_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instance_ready_reports
    ADD CONSTRAINT instance_ready_reports_instance_id_fkey FOREIGN KEY (instance_id) REFERENCES instances(instance_id) ON DELETE CASCADE;

--
-- Name: instances instances_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instances
    ADD CONSTRAINT instances_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE RESTRICT;

--
-- Name: instances instances_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY instances
    ADD CONSTRAINT instances_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT;

--
-- Name: operations operations_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY operations
    ADD CONSTRAINT operations_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES api_keys(api_key_id) ON DELETE CASCADE;

--
-- Name: operations operations_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY operations
    ADD CONSTRAINT operations_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE CASCADE;

--
-- Name: public_api_idempotency_keys public_api_idempotency_keys_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public_api_idempotency_keys
    ADD CONSTRAINT public_api_idempotency_keys_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES api_keys(api_key_id) ON DELETE CASCADE;

--
-- Name: ssh_keys ssh_keys_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY ssh_keys
    ADD CONSTRAINT ssh_keys_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE CASCADE;

--
-- Name: ssh_keys ssh_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY ssh_keys
    ADD CONSTRAINT ssh_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE CASCADE;

--
-- Name: users users_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY users
    ADD CONSTRAINT users_org_id_fkey FOREIGN KEY (org_id) REFERENCES organizations(organization_id) ON DELETE CASCADE;


-- Records one bootstrap env report per distinct content per instance, keeping
-- the newest 64 (called by the ready callback; see internal/db/ready_reports.go).
CREATE OR REPLACE FUNCTION save_instance_ready_report(p_instance text, p_report jsonb)
 RETURNS boolean
 LANGUAGE plpgsql
 SET lock_timeout TO '500ms'
AS $function$
DECLARE
    placed_provider TEXT;
    placed_upstream TEXT;
    fingerprint TEXT;
BEGIN
    SELECT upstream_provider, upstream_id INTO placed_provider, placed_upstream
      FROM instances WHERE instance_id=p_instance FOR UPDATE;
    IF NOT FOUND THEN RETURN FALSE; END IF;
    fingerprint := encode(digest(jsonb_build_object(
        'provider',placed_provider,'upstream',placed_upstream,'report',p_report)::text,'sha256'),'hex');
    INSERT INTO instance_ready_reports(instance_id,provider,upstream_id,report_hash,report)
      VALUES(p_instance,placed_provider,placed_upstream,fingerprint,p_report)
      ON CONFLICT(instance_id,report_hash) DO UPDATE SET
        last_observed_at=GREATEST(instance_ready_reports.last_observed_at,EXCLUDED.last_observed_at),
        callbacks_received=instance_ready_reports.callbacks_received+1;
    DELETE FROM instance_ready_reports WHERE report_id IN (
      SELECT report_id FROM instance_ready_reports WHERE instance_id=p_instance
        ORDER BY last_observed_at DESC,report_id DESC OFFSET 64);
    RETURN TRUE;
END;
$function$;
