package metrics

// Default is the process-wide registry. The predefined metrics below write to
// it, so callers just do metrics.SweepsTotal.Inc() without plumbing a registry.
var Default = NewRegistry()

// Predefined podsmedic metrics. Keeping them as package globals keeps wiring at
// the call sites a single line.
var (
	Up               = Default.NewGauge("podsmedic_up", "1 while the agent is running.")
	SweepsTotal      = Default.NewCounter("podsmedic_sweeps_total", "Cluster sweeps performed.")
	PodsScanned      = Default.NewGauge("podsmedic_pods_scanned", "Pods seen in the most recent sweep.")
	ProblemsDetected = Default.NewGauge("podsmedic_problems_detected", "Problems detected in the most recent sweep.")

	// AlertsTotal is labelled result=delivered|failed.
	AlertsTotal = Default.NewCounter("podsmedic_alerts_total", "Alerts by delivery result.", "result")

	// LLMRequests is labelled provider and result=ok|error.
	LLMRequests = Default.NewCounter("podsmedic_llm_requests_total", "Diagnosis requests by provider and result.", "provider", "result")
	// LLMLatency is labelled provider.
	LLMLatency = Default.NewHistogram("podsmedic_llm_latency_seconds", "Diagnosis request latency.",
		[]float64{0.5, 1, 2, 5, 10, 20, 30, 60}, "provider")
	// LLMTokens is labelled provider and kind=input|output|cache_read.
	LLMTokens = Default.NewCounter("podsmedic_llm_tokens_total", "Tokens consumed by diagnosis requests.", "provider", "kind")
	// LLMCost is labelled provider; zero unless per-token prices are configured.
	LLMCost = Default.NewFloatCounter("podsmedic_llm_cost_usd_total", "Estimated LLM spend in USD, from configured per-token prices.", "provider")

	// HealsTotal is labelled result=applied|dryrun|skipped|failed.
	HealsTotal = Default.NewCounter("podsmedic_heals_total", "Auto-heal attempts by outcome.", "result")
	// VerificationsTotal is labelled result=verified|rolledback.
	VerificationsTotal = Default.NewCounter("podsmedic_heal_verifications_total", "Heal verifications by verdict.", "result")
	// RollbacksTotal is labelled result=ok|failed.
	RollbacksTotal = Default.NewCounter("podsmedic_rollbacks_total", "Rollback executions by result.", "result")

	// SinkCheckFailures is labelled sink.
	SinkCheckFailures = Default.NewCounter("podsmedic_sink_check_failures_total", "Notification sink startup-check failures.", "sink")

	// Incident correlation.
	IncidentsTotal    = Default.NewCounter("podsmedic_incidents_total", "Incidents opened (one per correlated failure).")
	IncidentUpdates   = Default.NewCounter("podsmedic_incident_updates_total", "New symptoms added to an already-open incident.")
	IncidentsResolved = Default.NewCounter("podsmedic_incidents_resolved_total", "Incidents that cleared.")
	IncidentsOpen     = Default.NewGauge("podsmedic_incidents_open", "Currently open incidents.")

	// Circuit breaker.
	BreakerTripsTotal = Default.NewCounter("podsmedic_heal_breaker_trips_total", "Times the heal circuit breaker tripped open for a workload.")
	BreakerOpen       = Default.NewGauge("podsmedic_heal_breaker_open", "Workloads whose heal circuit breaker is currently open.")

	// Predictive heal.
	PredictionsTotal  = Default.NewCounter("podsmedic_predictions_total", "Predicted memory-pressure problems raised before an OOM kill.")
	PredictedPressure = Default.NewGauge("podsmedic_predicted_memory_pressure", "Containers with an active high-memory streak.")

	// Cluster capacity, as the heal validator sees it: free figures already have
	// the configured reserve held back, so alerting on these matches what a
	// scale-up will actually be allowed to consume.
	ClusterCPUFree          = Default.NewGauge("podsmedic_cluster_cpu_free_millicores", "Schedulable CPU headroom after the heal reserve.")
	ClusterMemoryFree       = Default.NewGauge("podsmedic_cluster_memory_free_bytes", "Schedulable memory headroom after the heal reserve.")
	ClusterPodSlotsFree     = Default.NewGauge("podsmedic_cluster_pod_slots_free", "Pod slots left on schedulable nodes after the heal reserve.")
	ClusterNodesSchedulable = Default.NewGauge("podsmedic_cluster_nodes_schedulable", "Nodes a new pod could actually be placed on.")

	// Rightsizing. Report-only, so there is nothing to count as applied — this
	// measures how much of the cluster has enough history to be judged.
	RightsizeTracked  = Default.NewGauge("podsmedic_rightsize_tracked", "Containers with a usage history under observation.")
	RightsizeFindings = Default.NewGauge("podsmedic_rightsize_findings", "Sizing suggestions the most recent report would make.")

	// Node health. Report-only: podsmedic never writes to a node, so these
	// measure what a human was told, not what was fixed. NodeFaultsTotal is
	// labelled kind.
	NodesWatched    = Default.NewGauge("podsmedic_nodes_watched", "Nodes examined in the most recent sweep.")
	NodeFaults      = Default.NewGauge("podsmedic_node_faults", "Node-level faults currently reported.")
	NodeFaultsTotal = Default.NewCounter("podsmedic_node_faults_total", "Node-level faults alerted on, by kind.", "kind")

	// Cluster-wide heal brakes.
	SurgeTrips = Default.NewCounter("podsmedic_heal_surge_trips_total", "Times healing was suspended cluster-wide because the failure pattern looked systemic.")

	// Telegram chat (inbound questions). ChatAnswers is labelled result=ok|error;
	// only free-form questions reach the model, so it also measures how much of
	// the chat traffic the local commands absorb for free.
	ChatAnswers = Default.NewCounter("podsmedic_chat_answers_total", "Operator questions answered by the model, by result.", "result")

	// Live view sign-ins, labelled result=ok|rejected|throttled. Rejected
	// attempts are the signal worth alerting on: the view has no business
	// receiving guesses.
	UILoginsTotal = Default.NewCounter("podsmedic_ui_logins_total", "Live-view sign-in attempts by result.", "result")

	// Playbook (learned heals replayed without an LLM diagnosis).
	PlaybookHitsTotal      = Default.NewCounter("podsmedic_playbook_hits_total", "Heals served from the playbook (no LLM diagnosis).")
	PlaybookRecordsTotal   = Default.NewCounter("podsmedic_playbook_records_total", "Verified heals learned into the playbook.")
	PlaybookEvictionsTotal = Default.NewCounter("podsmedic_playbook_evictions_total", "Remedies evicted after a replayed fix stopped holding.")
	PlaybookEntries        = Default.NewGauge("podsmedic_playbook_entries", "Remedies currently remembered in the playbook.")
	// Retirement. Retirements are routine hygiene; quarantines are not — a
	// quarantined workload is one automated healing has repeatedly failed to
	// fix, which is worth a human's attention.
	PlaybookRetirementsTotal = Default.NewCounter("podsmedic_playbook_retirements_total", "Remedies dropped for going too long without confirmation.")
	PlaybookQuarantinesTotal = Default.NewCounter("podsmedic_playbook_quarantines_total", "Times a workload+kind was barred from learning after repeated rollbacks.")
	PlaybookQuarantined      = Default.NewGauge("podsmedic_playbook_quarantined", "Workload+kind pairs currently barred from learning.")
)
