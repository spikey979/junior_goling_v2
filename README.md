# Rebuild unutar kontejnera
./scripts/rebuild_in_container.sh

# Pokretanje lokalno
- docker compose build
- docker compose up

ako se pojavi poruka:  /app/bin/aidispatcher: No such file or directory
treba pokrenuti: ./scripts/rebuild_in_container.sh, nakon čega: docker compose down i docker compose up


- Dashboard: http://localhost:8081/web/login (or your HOST_PORT)
- Health: http://localhost:8081/healthz
- Metrics: http://localhost:8081/metrics