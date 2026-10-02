package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	ReservationsConfirmed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "reservations_confirmed_total",
		Help: "Total number of confirmed reservations",
	})

	ReservationsDeclined = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reservations_declined_total",
		Help: "Total number of declined reservations by reason",
	}, []string{"reason"})

	SeatsAvailable = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seats_available",
		Help: "Number of available seats per show",
	}, []string{"show_id"})

	SeatsConfirmed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seats_confirmed",
		Help: "Number of confirmed seats per show",
	}, []string{"show_id"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests",
	}, []string{"method", "path", "status"})

	IdempotentReplays = promauto.NewCounter(prometheus.CounterOpts{
		Name: "idempotent_replays_total",
		Help: "Total idempotent replay responses",
	})
)
