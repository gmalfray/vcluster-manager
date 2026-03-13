#!/bin/bash
#
# generate-secret.sh
#
# Genere les secrets Kubernetes pour vcluster-manager :
#   - vcluster-manager-secrets : configuration (GitLab, Keycloak, OIDC)
#   - vcluster-manager-auth    : authentification locale (admin password, JWT)
#
# Le script ecrit le YAML sur stdout. Redirigez la sortie vers kubectl apply.
#
# Usage:
#   ./scripts/generate-secret.sh [--env preprod|prod] [--dry-run] [--namespace NS]
#
# Exemples:
#   # Generer et appliquer directement
#   ./scripts/generate-secret.sh --env preprod | kubectl apply -f -
#
#   # Generer dans un fichier
#   ./scripts/generate-secret.sh --env prod > secrets-prod.yaml
#
#   # Verifier sans generer
#   ./scripts/generate-secret.sh --env preprod --dry-run

set -euo pipefail

# --- Couleurs ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# --- Defaults ---
ENV=""
DRY_RUN=false
NAMESPACE="vcluster-manager"

# --- Parse args ---
while [[ $# -gt 0 ]]; do
    case "$1" in
        --env)       ENV="$2"; shift 2 ;;
        --dry-run)   DRY_RUN=true; shift ;;
        --namespace) NAMESPACE="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: $0 [--env preprod|prod] [--dry-run] [--namespace NS]"
            exit 0 ;;
        *)           echo "Option inconnue: $1"; exit 1 ;;
    esac
done

# --- Fonctions ---
prompt() {
    local var_name="$1"
    local prompt_text="$2"
    local default="$3"
    local is_secret="${4:-false}"

    if [[ -n "$default" ]]; then
        echo -en "${CYAN}${prompt_text}${NC} [${YELLOW}${default}${NC}]: " >&2
    else
        echo -en "${CYAN}${prompt_text}${NC}: " >&2
    fi

    if [[ "$is_secret" == "true" ]]; then
        read -rs value
        echo "" >&2
    else
        read -r value
    fi

    value="${value:-$default}"
    eval "$var_name='$value'"
}

prompt_choice() {
    local var_name="$1"
    local prompt_text="$2"
    shift 2
    local options=("$@")

    echo -e "${CYAN}${prompt_text}${NC}" >&2
    for i in "${!options[@]}"; do
        echo -e "  ${BOLD}$((i+1)))${NC} ${options[$i]}" >&2
    done
    echo -en "${CYAN}Choix${NC} [${YELLOW}1${NC}]: " >&2
    read -r choice
    choice="${choice:-1}"
    local idx=$((choice - 1))
    if [[ $idx -ge 0 && $idx -lt ${#options[@]} ]]; then
        eval "$var_name='${options[$idx]}'"
    else
        eval "$var_name='${options[0]}'"
    fi
}

# --- Header ---
echo "" >&2
echo -e "${BOLD}============================================${NC}" >&2
echo -e "${BOLD}  vCluster Manager - Generation des secrets${NC}" >&2
echo -e "${BOLD}============================================${NC}" >&2
echo "" >&2

# --- Environnement ---
if [[ -z "$ENV" ]]; then
    prompt_choice ENV "Environnement cible :" "preprod" "prod"
fi
echo -e "${GREEN}Environnement :${NC} ${ENV}" >&2
echo "" >&2

# --- GitLab ---
echo -e "${BOLD}--- GitLab ---${NC}" >&2
echo -e "${YELLOW}  Seul le token est sensible. Les IDs de projet vont dans le ConfigMap.${NC}" >&2
prompt GITLAB_TOKEN "Token GitLab (scope api)" "" true
echo "" >&2

# --- Keycloak Service Account ---
echo -e "${BOLD}--- Keycloak Service Account (gestion des clients OIDC ArgoCD) ---${NC}" >&2
echo -e "${YELLOW}  Seul le secret est sensible. KEYCLOAK_CLIENT_ID va dans le ConfigMap.${NC}" >&2
echo -e "${YELLOW}  Voir README.md section 'Compte de service Keycloak' pour la creation${NC}" >&2
prompt KEYCLOAK_CLIENT_SECRET "Client Secret Keycloak" "" true
echo "" >&2

# --- OIDC (auth de l'app) ---
echo -e "${BOLD}--- OIDC (authentification utilisateurs de l'app) ---${NC}" >&2
echo -e "${YELLOW}  Seul le secret est sensible. OIDC_CLIENT_ID et OIDC_REDIRECT_URL vont dans le ConfigMap.${NC}" >&2
echo -e "${YELLOW}  Voir README.md section 'Client OIDC' pour la creation${NC}" >&2
prompt OIDC_CLIENT_SECRET "Client secret OIDC" "" true
echo "" >&2

# --- Vault ---
echo -e "${BOLD}--- Vault (backend auth Kubernetes par vcluster) ---${NC}" >&2
echo -e "${YELLOW}  Seuls les credentials AppRole sont sensibles. VAULT_ADDR va dans le ConfigMap.${NC}" >&2
echo -e "${YELLOW}  Laisser vide pour desactiver la configuration automatique Vault${NC}" >&2
prompt VAULT_ROLE_ID   "AppRole role_id" "" false
prompt VAULT_SECRET_ID "AppRole secret_id" "" true
echo "" >&2

# --- Local admin auth (genere automatiquement) ---
ADMIN_PASSWORD=$(openssl rand -hex 12)
JWT_SECRET=$(openssl rand -base64 32)

# --- Resume ---
echo "" >&2
echo -e "${BOLD}============================================${NC}" >&2
echo -e "${BOLD}  Resume${NC}" >&2
echo -e "${BOLD}============================================${NC}" >&2
echo -e "  Namespace         : ${YELLOW}${NAMESPACE}${NC}" >&2
echo -e "  Environnement     : ${YELLOW}${ENV}${NC}" >&2
echo -e "" >&2
echo -e "  ${BOLD}Secret : vcluster-manager-secrets${NC}" >&2
echo -e "  GITLAB_TOKEN         : ${YELLOW}****${NC}" >&2
echo -e "  KEYCLOAK_CLIENT_SECRET: ${YELLOW}****${NC}" >&2
echo -e "  OIDC_CLIENT_SECRET   : ${YELLOW}****${NC}" >&2
echo -e "  VAULT_ROLE_ID        : ${YELLOW}${VAULT_ROLE_ID}${NC}" >&2
echo -e "  VAULT_SECRET_ID      : ${YELLOW}****${NC}" >&2
echo -e "" >&2
echo -e "  ${YELLOW}Les valeurs non sensibles (IDs, URLs) sont a configurer dans le ConfigMap.${NC}" >&2
echo -e "" >&2
echo -e "  ${BOLD}Secret : vcluster-manager-auth${NC}" >&2
echo -e "  ADMIN_PASSWORD    : ${YELLOW}${ADMIN_PASSWORD}${NC}" >&2
echo -e "  JWT_SECRET        : ${YELLOW}(genere)${NC}" >&2
echo -e "${BOLD}============================================${NC}" >&2
echo "" >&2

if $DRY_RUN; then
    echo -e "${YELLOW}[DRY-RUN] Aucun fichier genere.${NC}" >&2
    exit 0
fi

# --- Generation YAML ---
cat <<EOF
---
apiVersion: v1
kind: Secret
metadata:
  name: vcluster-manager-secrets
  namespace: ${NAMESPACE}
type: Opaque
stringData:
  GITLAB_TOKEN: "${GITLAB_TOKEN}"
  KEYCLOAK_CLIENT_SECRET: "${KEYCLOAK_CLIENT_SECRET}"
  OIDC_CLIENT_SECRET: "${OIDC_CLIENT_SECRET}"
  VAULT_ROLE_ID: "${VAULT_ROLE_ID}"
  VAULT_SECRET_ID: "${VAULT_SECRET_ID}"
---
apiVersion: v1
kind: Secret
metadata:
  name: vcluster-manager-auth
  namespace: ${NAMESPACE}
type: Opaque
stringData:
  ADMIN_PASSWORD: "${ADMIN_PASSWORD}"
  JWT_SECRET: "${JWT_SECRET}"
EOF

echo "" >&2
echo -e "${GREEN}Secrets generes sur stdout.${NC}" >&2
echo "" >&2
echo -e "Pour appliquer sur le cluster :" >&2
echo -e "  ${BOLD}./scripts/generate-secret.sh --env ${ENV} | kubectl apply -f -${NC}" >&2
echo "" >&2
echo -e "Pour retrouver le mot de passe admin :" >&2
echo -e "  ${BOLD}kubectl get secret vcluster-manager-auth -n ${NAMESPACE} -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d${NC}" >&2
echo "" >&2
echo -e "${YELLOW}Attention :${NC} Le namespace ${NAMESPACE} doit exister avant d'appliquer les secrets." >&2
