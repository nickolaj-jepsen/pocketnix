// The phone and the guest's power; go/internal/protocol/host.go is the contract.
package dev.nickolaj.pocketnix.agent

import org.json.JSONObject

/** One logind inhibitor that asks the phone to stay awake. */
data class WakeHolder(val who: String, val why: String)

/** The agent's wake push: whether the guest holds a sleep inhibitor, and who. */
data class Wake(val held: Boolean, val holders: List<WakeHolder>) {
    companion object {
        fun parse(o: JSONObject): Wake {
            val a = o.optJSONArray("holders")
            val holders = if (a == null) emptyList() else (0 until a.length()).map {
                val h = a.getJSONObject(it)
                WakeHolder(h.optString("who"), h.optString("why"))
            }
            return Wake(o.optBoolean("held"), holders)
        }
    }
}
