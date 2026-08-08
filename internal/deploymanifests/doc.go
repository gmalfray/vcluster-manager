// Package deploymanifests ne contient pas de code : il porte les tests qui
// vérifient les invariants des manifestes de deploy/base.
//
// Ces invariants ont un point commun — quand ils sont violés, rien n'échoue.
// Un fichier oublié dans kustomization.yaml n'est pas déployé, une policy sans
// binding ne s'applique à personne, et dans les deux cas le déploiement est
// vert. C'est le genre de défaut qu'aucun test d'application ne peut voir,
// parce qu'il ne porte pas sur le code mais sur ce qui part en production.
package deploymanifests
