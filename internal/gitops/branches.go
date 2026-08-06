package gitops

// Les deux branches du dépôt fluxprod, et pourquoi elles ne sont pas
// interchangeables.
//
// SourceBranch fait foi pour TOUS les environnements : chaque écriture y va, y
// compris les changements destinés à la prod (ADR-001). La promotion vers la prod
// se fait par une merge request vers DeployedBranch, jamais par un commit direct.
//
// ⚠️ Ne JAMAIS dériver ces valeurs de l'environnement. Le nom `preprod` est ici
// celui d'une branche Git, pas celui d'un environnement — la coïncidence entre les
// deux est purement lexicale. « Paramétrer proprement » ces sites par
// environnement enverrait les changements prod directement sur la branche que Flux
// prod surveille, en contournant la promotion.
//
// ADR-002 prévoit qu'à terme la promotion disparaisse et qu'il ne reste qu'une
// branche ; d'ici là ces constantes servent aussi de carte des sites que cette
// migration devra revisiter.
const (
	// SourceBranch est la branche fluxprod qui fait foi, en écriture.
	SourceBranch = "preprod"
	// DeployedBranch reflète ce qui tourne réellement en prod. Lecture seule.
	DeployedBranch = "master"
)
