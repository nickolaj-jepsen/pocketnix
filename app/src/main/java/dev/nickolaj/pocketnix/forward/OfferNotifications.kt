package dev.nickolaj.pocketnix.forward

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.gui.guard
import dev.nickolaj.pocketnix.vm

/** Posts and withdraws [ForwardOffers]' offers; Allow, Dismiss and a swipe go through [ForwardOfferReceiver]. */
class OfferNotifications(private val context: Context) {
    private val manager = context.getSystemService(NotificationManager::class.java)

    init {
        manager.createNotificationChannel(NotificationChannel(CHANNEL, context.getString(R.string.forward_offers_channel), NotificationManager.IMPORTANCE_DEFAULT))
    }

    /** Offers [offer], naming [phonePort] (null: none is free) as where Allow would forward it. */
    fun post(offer: ForwardOffers.Offer, phonePort: Int?, lan: Boolean, hostname: String?) {
        if (context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val port = offer.port
        val where = when {
            phonePort == null -> "No phone port is free for it."
            lan -> "Allow forwards phone port $phonePort, on every network, to it."
            else -> "Allow forwards phone port $phonePort to it."
        }
        val text = listOfNotNull(offer.failure, where).joinToString(" ")
        val b = Notification.Builder(context, CHANNEL)
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle("Guest port $port is listening")
            .setContentText(text)
            .setStyle(Notification.BigTextStyle().bigText(text))
            .setSubText(hostname)
            .setDeleteIntent(answer(port, ForwardOfferReceiver.ACTION_DISMISS))
            .setOnlyAlertOnce(true)
            .addAction(Notification.Action.Builder(null, "Allow", answer(port, ForwardOfferReceiver.ACTION_ALLOW)).build())
            .addAction(Notification.Action.Builder(null, "Dismiss", answer(port, ForwardOfferReceiver.ACTION_DISMISS)).build())
        manager.notify(TAG, port, b.build())
    }

    fun cancel(port: Int) = manager.cancel(TAG, port)

    private fun answer(port: Int, action: String): PendingIntent {
        val i = Intent(context, ForwardOfferReceiver::class.java)
            .setAction(action)
            // PendingIntents differ by data, not extras: one per port and answer.
            .setData(Uri.Builder().scheme("pocketnix-offer").authority(port.toString()).appendPath(action).build())
            .putExtra(ForwardOfferReceiver.EXTRA_PORT, port)
        return PendingIntent.getBroadcast(context, 0, i, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }

    private companion object {
        // Tagged, so ports don't collide with the app's other notification ids.
        const val TAG = "forward-offer"
        const val CHANNEL = "forward-offers"
    }
}

/** An offer's Allow, Dismiss or swipe; not exported, only its own PendingIntents reach it. */
class ForwardOfferReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) = guard("forward offer ${intent.action}") {
        val port = intent.getIntExtra(EXTRA_PORT, 0)
        if (port <= 0) return@guard
        when (intent.action) {
            ACTION_ALLOW -> context.vm.answerOfferLater(port, allow = true)
            ACTION_DISMISS -> context.vm.answerOfferLater(port, allow = false)
        }
    }

    companion object {
        const val ACTION_ALLOW = "dev.nickolaj.pocketnix.action.FORWARD_OFFER_ALLOW"
        const val ACTION_DISMISS = "dev.nickolaj.pocketnix.action.FORWARD_OFFER_DISMISS"
        const val EXTRA_PORT = "dev.nickolaj.pocketnix.extra.PORT"
    }
}
